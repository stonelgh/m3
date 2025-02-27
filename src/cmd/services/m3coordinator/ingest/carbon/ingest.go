// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// Package ingestcarbon implements a carbon ingester.
package ingestcarbon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/m3db/m3/src/cmd/services/m3coordinator/downsample"
	"github.com/m3db/m3/src/cmd/services/m3coordinator/ingest"
	"github.com/m3db/m3/src/cmd/services/m3query/config"
	"github.com/m3db/m3/src/metrics/aggregation"
	"github.com/m3db/m3/src/metrics/carbon"
	"github.com/m3db/m3/src/metrics/policy"
	"github.com/m3db/m3/src/query/graphite/graphite"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage/m3"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/ts"
	"github.com/m3db/m3/src/x/instrument"
	"github.com/m3db/m3/src/x/pool"
	m3xserver "github.com/m3db/m3/src/x/server"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/uber-go/tally"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	maxResourcePoolNameSize = 1024
	maxPooledTagsSize       = 16
	defaultResourcePoolSize = 4096
)

var (
	// Used for parsing carbon names into tags.
	carbonSeparatorByte  = byte('.')
	carbonSeparatorBytes = []byte{carbonSeparatorByte}

	errCannotGenerateTagsFromEmptyName = errors.New("cannot generate tags from empty name")
	errIOptsMustBeSet                  = errors.New("carbon ingester options: instrument options must be st")
	errWorkerPoolMustBeSet             = errors.New("carbon ingester options: worker pool must be set")
	errMultipleWorkerPoolsSet          = errors.New("carbon ingester options: only single worker pool can be set")
)

// Options configures the ingester.
type Options struct {
	InstrumentOptions instrument.Options
	StaticWorkerPool  xsync.StaticPooledWorkerPool
	DynamicWorkerPool xsync.DynamicPooledWorkerPool
	IngesterConfig    config.CarbonIngesterConfiguration
}

// CarbonIngesterRules contains the carbon ingestion rules.
type CarbonIngesterRules struct {
	Rules []config.CarbonIngesterRuleConfiguration
}

// Validate validates the options struct.
func (o *Options) Validate() error {
	if o.InstrumentOptions == nil {
		return errIOptsMustBeSet
	}
	if o.StaticWorkerPool == nil && o.DynamicWorkerPool == nil {
		return errWorkerPoolMustBeSet
	}
	if o.StaticWorkerPool != nil && o.DynamicWorkerPool != nil {
		return errMultipleWorkerPoolsSet
	}
	return nil
}

// NewIngester returns an ingester for carbon metrics.
func NewIngester(
	downsamplerAndWriter ingest.DownsamplerAndWriter,
	clusterNamespacesWatcher m3.ClusterNamespacesWatcher,
	opts Options,
) (m3xserver.Handler, error) {
	err := opts.Validate()
	if err != nil {
		return nil, err
	}

	tagOpts := models.NewTagOptions().SetIDSchemeType(models.TypeGraphite)
	err = tagOpts.Validate()
	if err != nil {
		return nil, err
	}

	poolOpts := pool.NewObjectPoolOptions().
		SetInstrumentOptions(opts.InstrumentOptions).
		SetRefillLowWatermark(0).
		SetRefillHighWatermark(0).
		SetSize(defaultResourcePoolSize)

	resourcePool := pool.NewObjectPool(poolOpts)
	resourcePool.Init(func() interface{} {
		return &lineResources{
			name:       make([]byte, 0, maxResourcePoolNameSize),
			datapoints: make([]ts.Datapoint, 1),
			tags:       make([]models.Tag, 0, maxPooledTagsSize),
		}
	})

	scope := opts.InstrumentOptions.MetricsScope()
	metrics, err := newCarbonIngesterMetrics(scope)
	if err != nil {
		return nil, err
	}

	ingester := &ingester{
		downsamplerAndWriter: downsamplerAndWriter,
		opts:                 opts,
		logger:               opts.InstrumentOptions.Logger(),
		tagOpts:              tagOpts,
		metrics:              metrics,
		lineResourcesPool:    resourcePool,
	}
	// No need to retain watch as NamespaceWatcher.Close() will handle closing any watches
	// generated by creating listeners.
	clusterNamespacesWatcher.RegisterListener(ingester)

	return ingester, nil
}

type ingester struct {
	downsamplerAndWriter ingest.DownsamplerAndWriter
	opts                 Options
	logger               *zap.Logger
	metrics              carbonIngesterMetrics
	tagOpts              models.TagOptions

	lineResourcesPool pool.ObjectPool

	sync.RWMutex
	rules []ruleAndMatcher
}

func (i *ingester) OnUpdate(clusterNamespaces m3.ClusterNamespaces) {
	i.Lock()
	defer i.Unlock()

	rules := i.regenerateIngestionRulesWithLock(clusterNamespaces)
	if rules == nil {
		namespaces := make([]string, 0, len(clusterNamespaces))
		for _, ns := range clusterNamespaces {
			namespaces = append(namespaces, ns.NamespaceID().String())
		}
		i.logger.Warn("generated empty carbon ingestion rules from latest cluster namespaces update. leaving"+
			" current set of rules as-is.", zap.Strings("namespaces", namespaces))
		return
	}

	compiledRules, err := i.compileRulesWithLock(*rules)
	if err != nil {
		i.logger.Error("failed to compile latest rules. continuing to use existing carbon ingestion "+
			"rules", zap.Error(err))
		return
	}

	i.rules = compiledRules
}

func (i *ingester) regenerateIngestionRulesWithLock(clusterNamespaces m3.ClusterNamespaces) *CarbonIngesterRules {
	var (
		rules = &CarbonIngesterRules{
			Rules: i.opts.IngesterConfig.RulesOrDefault(clusterNamespaces),
		}
		namespacesByRetention = make(map[m3.RetentionResolution]m3.ClusterNamespace, len(clusterNamespaces))
	)

	for _, ns := range clusterNamespaces {
		if ns.Options().Attributes().MetricsType == storagemetadata.AggregatedMetricsType {
			resRet := m3.RetentionResolution{
				Resolution: ns.Options().Attributes().Resolution,
				Retention:  ns.Options().Attributes().Retention,
			}
			// This should never happen
			if _, ok := namespacesByRetention[resRet]; ok {
				i.logger.Error(
					"cannot have namespaces with duplicate resolution and retention",
					zap.String("resolution", resRet.Resolution.String()),
					zap.String("retention", resRet.Retention.String()))
				return nil
			}

			namespacesByRetention[resRet] = ns
		}
	}

	// Validate rule policies.
	for _, rule := range rules.Rules {
		// Sort so we can detect duplicates.
		sort.Slice(rule.Policies, func(i, j int) bool {
			if rule.Policies[i].Resolution == rule.Policies[j].Resolution {
				return rule.Policies[i].Retention < rule.Policies[j].Retention
			}

			return rule.Policies[i].Resolution < rule.Policies[j].Resolution
		})

		var lastPolicy config.CarbonIngesterStoragePolicyConfiguration
		for idx, policy := range rule.Policies {
			if policy == lastPolicy {
				i.logger.Error(
					"cannot include the same storage policy multiple times for a single carbon ingestion rule",
					zap.String("pattern", rule.Pattern),
					zap.String("resolution", policy.Resolution.String()),
					zap.String("retention", policy.Retention.String()))
				return nil
			}

			if idx > 0 && !rule.Aggregation.EnabledOrDefault() && policy.Resolution != lastPolicy.Resolution {
				i.logger.Error(
					"cannot include multiple storage policies with different resolutions if aggregation is disabled",
					zap.String("pattern", rule.Pattern),
					zap.String("resolution", policy.Resolution.String()),
					zap.String("retention", policy.Retention.String()))
				return nil
			}

			_, ok := namespacesByRetention[m3.RetentionResolution{
				Resolution: policy.Resolution,
				Retention:  policy.Retention,
			}]

			// Disallow storage policies that don't match any known M3DB clusters.
			if !ok {
				i.logger.Error(
					"cannot enable carbon ingestion without a corresponding aggregated M3DB namespace",
					zap.String("resolution", policy.Resolution.String()), zap.String("retention", policy.Retention.String()))
				return nil
			}

			lastPolicy = policy
		}
	}

	if len(rules.Rules) == 0 {
		i.logger.Warn("no carbon ingestion rules were provided and no aggregated M3DB namespaces exist, carbon metrics will not be ingested")
		return nil
	}

	if len(i.opts.IngesterConfig.Rules) == 0 {
		i.logger.Info("no carbon ingestion rules were provided, all carbon metrics will be written to all aggregated M3DB namespaces")
	}

	return rules
}

func (i *ingester) Handle(conn net.Conn) {
	var (
		// Interfaces require a context be passed, but M3DB client already has timeouts
		// built in and allocating a new context each time is expensive so we just pass
		// the same context always and rely on M3DB client timeouts.
		ctx     = context.Background()
		wg      = sync.WaitGroup{}
		s       = carbon.NewScanner(conn, i.opts.InstrumentOptions)
		logger  = i.opts.InstrumentOptions.Logger()
		rewrite = &i.opts.IngesterConfig.Rewrite
	)

	logger.Debug("handling new carbon ingestion connection")
	for s.Scan() {
		received := time.Now()
		name, timestamp, value := s.Metric()

		resources := i.getLineResources()

		// Copy name since scanner bytes are recycled.
		resources.name = copyAndRewrite(resources.name, name, rewrite)

		wg.Add(1)
		work := func() {
			ok := i.write(ctx, resources, xtime.ToUnixNano(timestamp), value)
			if ok {
				i.metrics.success.Inc(1)
			}

			now := time.Now()

			// Always record age regardless of success/failure since
			// sometimes errors can be due to how old the metrics are
			// and not recording age would obscure this visibility from
			// the metrics of how fresh/old the incoming metrics are.
			age := now.Sub(timestamp)
			i.metrics.ingestLatency.RecordDuration(age)

			// Also record write latency (not relative to metric timestamp).
			i.metrics.writeLatency.RecordDuration(now.Sub(received))

			// The contract is that after the DownsamplerAndWriter returns, any resources
			// that it needed to hold onto have already been copied.
			i.putLineResources(resources)
			wg.Done()
		}
		if i.opts.StaticWorkerPool != nil {
			i.opts.StaticWorkerPool.Go(work)
		} else {
			i.opts.DynamicWorkerPool.GoAlways(work)
		}

		i.metrics.malformed.Inc(int64(s.MalformedCount))
		s.MalformedCount = 0
	}

	if err := s.Err(); err != nil {
		logger.Error("encountered error during carbon ingestion when scanning connection", zap.Error(err))
	}

	logger.Debug("waiting for outstanding carbon ingestion writes to complete")
	wg.Wait()
	logger.Debug("all outstanding writes completed, shutting down carbon ingestion handler")

	// Don't close the connection, that is the server's responsibility.
}

func (i *ingester) write(
	ctx context.Context,
	resources *lineResources,
	timestamp xtime.UnixNano,
	value float64,
) bool {
	downsampleAndStoragePolicies := ingest.WriteOptions{
		// Set both of these overrides to true to indicate that only the exact mapping
		// rules and storage policies that we provide should be used and that all
		// default behavior (like performing all possible downsamplings and writing
		// all data to the unaggregated namespace in storage) should be ignored.
		DownsampleOverride: true,
		WriteOverride:      true,
	}

	matched := 0
	defer func() {
		if matched == 0 {
			// No policies matched.
			debugLog := i.logger.Check(zapcore.DebugLevel, "no rules matched carbon metric, skipping")
			if debugLog != nil {
				debugLog.Write(zap.ByteString("name", resources.name))
			}
			return
		}

		debugLog := i.logger.Check(zapcore.DebugLevel, "successfully wrote carbon metric")
		if debugLog != nil {
			debugLog.Write(zap.ByteString("name", resources.name),
				zap.Int("matchedRules", matched))
		}
	}()

	i.RLock()
	rules := i.rules
	i.RUnlock()

	for _, rule := range rules {
		var matches bool
		switch {
		case rule.rule.Pattern == graphite.MatchAllPattern:
			matches = true
		case rule.regexp != nil:
			matches = rule.regexp.Match(resources.name)
		case len(rule.contains) != 0:
			matches = bytes.Contains(resources.name, rule.contains)
		}

		if matches {
			// Each rule should only have either mapping rules or storage policies so
			// one of these should be a no-op.
			downsampleAndStoragePolicies.DownsampleMappingRules = rule.mappingRules
			downsampleAndStoragePolicies.WriteStoragePolicies = rule.storagePolicies

			debugLog := i.logger.Check(zapcore.DebugLevel, "carbon metric matched by pattern")
			if debugLog != nil {
				debugLog.Write(zap.ByteString("name", resources.name),
					zap.String("pattern", rule.rule.Pattern),
					zap.Any("mappingRules", rule.mappingRules),
					zap.Any("storagePolicies", rule.storagePolicies))
			}

			// Break because we only want to apply one rule per metric based on which
			// ever one matches first.
			err := i.writeWithOptions(ctx, resources, timestamp, value,
				downsampleAndStoragePolicies)
			if err != nil {
				return false
			}

			matched++

			// If continue is not specified then we matched the current set of rules.
			if !rule.rule.Continue {
				break
			}
		}
	}

	return matched > 0
}

func (i *ingester) writeWithOptions(
	ctx context.Context,
	resources *lineResources,
	timestamp xtime.UnixNano,
	value float64,
	opts ingest.WriteOptions,
) error {
	resources.datapoints[0] = ts.Datapoint{Timestamp: timestamp, Value: value}
	tags, err := GenerateTagsFromNameIntoSlice(resources.name, i.tagOpts, resources.tags)
	if err != nil {
		i.logger.Error("err generating tags from carbon",
			zap.String("name", string(resources.name)), zap.Error(err))
		i.metrics.malformed.Inc(1)
		return err
	}

	err = i.downsamplerAndWriter.Write(ctx, tags, resources.datapoints,
		xtime.Second, nil, opts)
	if err != nil {
		i.logger.Error("err writing carbon metric",
			zap.String("name", string(resources.name)), zap.Error(err))
		i.metrics.err.Inc(1)
		return err
	}

	return nil
}

func (i *ingester) Close() {
	// We don't maintain any state in-between connections so there is nothing to do here.
}

type carbonIngesterMetrics struct {
	success       tally.Counter
	err           tally.Counter
	malformed     tally.Counter
	ingestLatency tally.Histogram
	writeLatency  tally.Histogram
}

func newCarbonIngesterMetrics(scope tally.Scope) (carbonIngesterMetrics, error) {
	buckets, err := ingest.NewLatencyBuckets()
	if err != nil {
		return carbonIngesterMetrics{}, err
	}
	return carbonIngesterMetrics{
		success:       scope.Counter("success"),
		err:           scope.Counter("error"),
		malformed:     scope.Counter("malformed"),
		writeLatency:  scope.SubScope("write").Histogram("latency", buckets.WriteLatencyBuckets),
		ingestLatency: scope.SubScope("ingest").Histogram("latency", buckets.IngestLatencyBuckets),
	}, nil
}

// GenerateTagsFromName accepts a carbon metric name and blows it up into a list of
// key-value pair tags such that an input like:
//      foo.bar.baz
// becomes
//      __g0__:foo
//      __g1__:bar
//      __g2__:baz
func GenerateTagsFromName(
	name []byte,
	opts models.TagOptions,
) (models.Tags, error) {
	return generateTagsFromName(name, opts, nil)
}

// GenerateTagsFromNameIntoSlice does the same thing as GenerateTagsFromName except
// it allows the caller to provide the slice into which the tags are appended.
func GenerateTagsFromNameIntoSlice(
	name []byte,
	opts models.TagOptions,
	tags []models.Tag,
) (models.Tags, error) {
	return generateTagsFromName(name, opts, tags)
}

func generateTagsFromName(
	name []byte,
	opts models.TagOptions,
	tags []models.Tag,
) (models.Tags, error) {
	if len(name) == 0 {
		return models.EmptyTags(), errCannotGenerateTagsFromEmptyName
	}

	numTags := bytes.Count(name, carbonSeparatorBytes) + 1

	if cap(tags) >= numTags {
		tags = tags[:0]
	} else {
		tags = make([]models.Tag, 0, numTags)
	}

	startIdx := 0
	tagNum := 0
	for i, charByte := range name {
		if charByte == carbonSeparatorByte {
			if i+1 < len(name) && name[i+1] == carbonSeparatorByte {
				return models.EmptyTags(),
					fmt.Errorf("carbon metric: %s has duplicate separator", string(name))
			}

			tags = append(tags, models.Tag{
				Name:  graphite.TagName(tagNum),
				Value: name[startIdx:i],
			})
			startIdx = i + 1
			tagNum++
		}
	}

	// Write out the final tag since the for loop above will miss anything
	// after the final separator. Note, that we make sure that the final
	// character in the name is not the separator because in that case there
	// would be no additional tag to add. I.E if the input was:
	//      foo.bar.baz
	// then the for loop would append foo and bar, but we would still need to
	// append baz, however, if the input was:
	//      foo.bar.baz.
	// then the foor loop would have appended foo, bar, and baz already.
	if name[len(name)-1] != carbonSeparatorByte {
		tags = append(tags, models.Tag{
			Name:  graphite.TagName(tagNum),
			Value: name[startIdx:],
		})
	}

	return models.Tags{Opts: opts, Tags: tags}, nil
}

// Compile all the carbon ingestion rules into matcher so that we can
// perform matching. Also, generate all the mapping rules and storage
// policies that we will need to pass to the DownsamplerAndWriter upfront
// so that we don't need to create them each time.
//
// Note that only one rule will be applied per metric and rules are applied
// such that the first one that matches takes precedence. As a result we need
// to make sure to maintain the order of the rules when we generate the compiled ones.
func (i *ingester) compileRulesWithLock(rules CarbonIngesterRules) ([]ruleAndMatcher, error) {
	compiledRules := make([]ruleAndMatcher, 0, len(rules.Rules))
	for _, rule := range rules.Rules {
		if rule.Pattern != "" && rule.Contains != "" {
			return nil, fmt.Errorf(
				"rule contains both pattern and contains: pattern=%s, contains=%s",
				rule.Pattern, rule.Contains)
		}

		var (
			contains []byte
			compiled *regexp.Regexp
		)
		if rule.Contains != "" {
			contains = []byte(rule.Contains)
		} else {
			var err error
			compiled, err = regexp.Compile(rule.Pattern)
			if err != nil {
				return nil, err
			}
		}

		storagePolicies := make([]policy.StoragePolicy, 0, len(rule.Policies))
		for _, currPolicy := range rule.Policies {
			storagePolicy := policy.NewStoragePolicy(
				currPolicy.Resolution, xtime.Second, currPolicy.Retention)
			storagePolicies = append(storagePolicies, storagePolicy)
		}

		compiledRule := ruleAndMatcher{
			rule:     rule,
			contains: contains,
			regexp:   compiled,
		}

		if rule.Aggregation.EnabledOrDefault() {
			compiledRule.mappingRules = []downsample.AutoMappingRule{
				{
					Aggregations: []aggregation.Type{rule.Aggregation.TypeOrDefault()},
					Policies:     storagePolicies,
				},
			}
		} else {
			compiledRule.storagePolicies = storagePolicies
		}
		compiledRules = append(compiledRules, compiledRule)
	}

	return compiledRules, nil
}

func (i *ingester) getLineResources() *lineResources {
	return i.lineResourcesPool.Get().(*lineResources)
}

func (i *ingester) putLineResources(l *lineResources) {
	tooLargeForPool := cap(l.name) > maxResourcePoolNameSize ||
		len(l.datapoints) > 1 || // We always write one datapoint at a time.
		cap(l.datapoints) > 1 ||
		cap(l.tags) > maxPooledTagsSize

	if tooLargeForPool {
		return
	}

	// Reset.
	l.name = l.name[:0]
	l.datapoints[0] = ts.Datapoint{}
	for i := range l.tags {
		// Free pointers.
		l.tags[i] = models.Tag{}
	}
	l.tags = l.tags[:0]

	i.lineResourcesPool.Put(l)
}

type lineResources struct {
	name       []byte
	datapoints []ts.Datapoint
	tags       []models.Tag
}

type ruleAndMatcher struct {
	rule            config.CarbonIngesterRuleConfiguration
	regexp          *regexp.Regexp
	contains        []byte
	mappingRules    []downsample.AutoMappingRule
	storagePolicies []policy.StoragePolicy
}
