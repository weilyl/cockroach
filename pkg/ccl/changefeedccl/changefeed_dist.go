// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdceval"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/errors"
)

func init() {
	rowexec.NewChangeAggregatorProcessor = newChangeAggregatorProcessor
	rowexec.NewChangeFrontierProcessor = newChangeFrontierProcessor
}

const (
	changeAggregatorProcName = `changeagg`
	changeFrontierProcName   = `changefntr`
)

// distChangefeedFlow plans and runs a distributed changefeed.
//
// One or more ChangeAggregator processors watch table data for changes. These
// transform the changed kvs into changed rows and either emit them to a sink
// (such as kafka) or, if there is no sink, forward them in columns 1,2,3 (where
// they will be eventually returned directly via pgwire). In either case,
// periodically a span will become resolved as of some timestamp, meaning that
// no new rows will ever be emitted at or below that timestamp. These span-level
// resolved timestamps are emitted as a marshaled `jobspb.ResolvedSpan` proto in
// column 0.
//
// The flow will always have exactly one ChangeFrontier processor which all the
// ChangeAggregators feed into. It collects all span-level resolved timestamps
// and aggregates them into a changefeed-level resolved timestamp, which is the
// minimum of the span-level resolved timestamps. This changefeed-level resolved
// timestamp is emitted into the changefeed sink (or returned to the gateway if
// there is no sink) whenever it advances. ChangeFrontier also updates the
// progress of the changefeed's corresponding system job.
func distChangefeedFlow(
	ctx context.Context,
	execCtx sql.JobExecContext,
	jobID jobspb.JobID,
	details jobspb.ChangefeedDetails,
	progress jobspb.Progress,
	resultsCh chan<- tree.Datums,
) error {

	opts := changefeedbase.MakeStatementOptions(details.Opts)

	// NB: A non-empty high water indicates that we have checkpointed a resolved
	// timestamp. Skipping the initial scan is equivalent to starting the
	// changefeed from a checkpoint at its start time. Initialize the progress
	// based on whether we should perform an initial scan.
	{
		h := progress.GetHighWater()
		noHighWater := (h == nil || h.IsEmpty())
		// We want to set the highWater and thus avoid an initial scan if either
		// this is a cursor and there was no request for one, or we don't have a
		// cursor but we have a request to not have an initial scan.
		initialScanType, err := opts.GetInitialScanType()
		if err != nil {
			return err
		}
		if noHighWater && initialScanType == changefeedbase.NoInitialScan {
			// If there is a cursor, the statement time has already been set to it.
			progress.Progress = &jobspb.Progress_HighWater{HighWater: &details.StatementTime}
		}
	}

	var initialHighWater hlc.Timestamp
	schemaTS := details.StatementTime
	{
		if h := progress.GetHighWater(); h != nil && !h.IsEmpty() {
			initialHighWater = *h
			// If we have a high-water set, use it to compute the spans, since the
			// ones at the statement time may have been garbage collected by now.
			schemaTS = initialHighWater
		}

		// We want to fetch the target spans as of the timestamp following the
		// highwater unless the highwater corresponds to a timestamp of an initial
		// scan. This logic is irritatingly complex but extremely important. Namely,
		// we may be here because the schema changed at the current resolved
		// timestamp. However, an initial scan should be performed at exactly the
		// timestamp specified; initial scans can be created at the timestamp of a
		// schema change and thus should see the side-effect of the schema change.
		isRestartAfterCheckpointOrNoInitialScan := progress.GetHighWater() != nil
		if isRestartAfterCheckpointOrNoInitialScan {
			schemaTS = schemaTS.Next()
		}
	}

	var checkpoint jobspb.ChangefeedProgress_Checkpoint
	if cf := progress.GetChangefeed(); cf != nil && cf.Checkpoint != nil {
		checkpoint = *cf.Checkpoint
	}

	return startDistChangefeed(
		ctx, execCtx, jobID, schemaTS, details, initialHighWater, checkpoint, resultsCh)
}

func fetchTableDescriptors(
	ctx context.Context,
	execCfg *sql.ExecutorConfig,
	targets changefeedbase.Targets,
	ts hlc.Timestamp,
) ([]catalog.TableDescriptor, error) {
	var targetDescs []catalog.TableDescriptor

	fetchSpans := func(
		ctx context.Context, txn *kv.Txn, descriptors *descs.Collection,
	) error {
		targetDescs = make([]catalog.TableDescriptor, 0, targets.NumUniqueTables())
		if err := txn.SetFixedTimestamp(ctx, ts); err != nil {
			return err
		}
		// Note that all targets are currently guaranteed to have a Table ID
		// and lie within the primary index span. Deduplication is important
		// here as requesting the same span twice will deadlock.
		return targets.EachTableID(func(id catid.DescID) error {
			flags := tree.ObjectLookupFlagsWithRequired()
			flags.AvoidLeased = true
			tableDesc, err := descriptors.GetImmutableTableByID(ctx, txn, id, flags)
			if err != nil {
				return err
			}
			targetDescs = append(targetDescs, tableDesc)
			return nil
		})
	}
	if err := sql.DescsTxn(ctx, execCfg, fetchSpans); err != nil {
		return nil, err
	}
	return targetDescs, nil
}

// changefeedResultTypes is the types returned by changefeed stream.
var changefeedResultTypes = []*types.T{
	types.Bytes,  // aggregator progress update
	types.String, // topic
	types.Bytes,  // key
	types.Bytes,  // value
}

// fetchSpansForTable returns the set of spans for the specified table.
// Usually, this is just the primary index span.
// However, if details.Select is not empty, the set of spans returned may be
// restricted to satisfy predicate in the select clause.  In that case,
// possibly updated select clause returned representing the remaining expression
// that still needs to be applied to the events.
func fetchSpansForTables(
	ctx context.Context,
	execCtx sql.JobExecContext,
	tableDescs []catalog.TableDescriptor,
	details jobspb.ChangefeedDetails,
) (_ []roachpb.Span, updatedExpression string, _ error) {
	var trackedSpans []roachpb.Span
	if details.Select == "" {
		for _, d := range tableDescs {
			trackedSpans = append(trackedSpans, d.PrimaryIndexSpan(execCtx.ExecCfg().Codec))
		}
		return trackedSpans, "", nil
	}

	if len(tableDescs) != 1 {
		return nil, "", pgerror.Newf(pgcode.InvalidParameterValue,
			"filter can only be used with single target (found %d)",
			len(tableDescs))
	}
	target := details.TargetSpecifications[0]
	includeVirtual := details.Opts[changefeedbase.OptVirtualColumns] == string(changefeedbase.OptVirtualColumnsNull)
	return cdceval.ConstrainPrimaryIndexSpanByFilter(
		ctx, execCtx, details.Select, tableDescs[0], target, includeVirtual)
}

var replanChangefeedThreshold = settings.RegisterFloatSetting(
	settings.TenantWritable,
	"changefeed.replan_flow_threshold",
	"fraction of initial flow instances that would be added or updated above which a redistribution would occur (0=disabled)",
	0.0,
)

var replanChangefeedFrequency = settings.RegisterDurationSetting(
	settings.TenantWritable,
	"changefeed.replan_flow_frequency",
	"frequency at which changefeed checks to see if redistributing would change its physical execution plan",
	10*time.Minute,
	settings.PositiveDuration,
)

// startDistChangefeed starts distributed changefeed execution.
func startDistChangefeed(
	ctx context.Context,
	execCtx sql.JobExecContext,
	jobID jobspb.JobID,
	schemaTS hlc.Timestamp,
	details jobspb.ChangefeedDetails,
	initialHighWater hlc.Timestamp,
	checkpoint jobspb.ChangefeedProgress_Checkpoint,
	resultsCh chan<- tree.Datums,
) error {
	execCfg := execCtx.ExecCfg()
	tableDescs, err := fetchTableDescriptors(ctx, execCfg, AllTargets(details), schemaTS)
	if err != nil {
		return err
	}
	trackedSpans, selectClause, err := fetchSpansForTables(ctx, execCtx, tableDescs, details)
	if err != nil {
		return err
	}
	cfKnobs := execCfg.DistSQLSrv.TestingKnobs.Changefeed

	// Changefeed flows handle transactional consistency themselves.
	var noTxn *kv.Txn

	dsp := execCtx.DistSQLPlanner()
	evalCtx := execCtx.ExtendedEvalContext()

	p, planCtx, err := makePlan(execCtx, jobID, details, initialHighWater, checkpoint, trackedSpans, selectClause)(ctx, dsp)
	if err != nil {
		return err
	}

	replanOracle := sql.ReplanOnChangedFraction(
		func() float64 {
			return replanChangefeedThreshold.Get(execCtx.ExecCfg().SV())
		},
	)
	if knobs, ok := cfKnobs.(*TestingKnobs); ok && knobs != nil && knobs.ShouldReplan != nil {
		replanOracle = knobs.ShouldReplan
	}

	replanner, stopReplanner := sql.PhysicalPlanChangeChecker(ctx,
		p,
		makePlan(execCtx, jobID, details, initialHighWater, checkpoint, trackedSpans, selectClause),
		execCtx,
		replanOracle,
		func() time.Duration { return replanChangefeedFrequency.Get(execCtx.ExecCfg().SV()) },
	)

	execPlan := func(ctx context.Context) error {
		defer stopReplanner()

		resultRows := makeChangefeedResultWriter(resultsCh)
		recv := sql.MakeDistSQLReceiver(
			ctx,
			resultRows,
			tree.Rows,
			execCtx.ExecCfg().RangeDescriptorCache,
			noTxn,
			nil, /* clockUpdater */
			evalCtx.Tracing,
			execCtx.ExecCfg().ContentionRegistry,
			nil, /* testingPushCallback */
		)
		defer recv.Release()

		var finishedSetupFn func()
		if details.SinkURI != `` {
			// We abuse the job's results channel to make CREATE CHANGEFEED wait for
			// this before returning to the user to ensure the setup went okay. Job
			// resumption doesn't have the same hack, but at the moment ignores
			// results and so is currently okay. Return nil instead of anything
			// meaningful so that if we start doing anything with the results
			// returned by resumed jobs, then it breaks instead of returning
			// nonsense.
			finishedSetupFn = func() { resultsCh <- tree.Datums(nil) }
		}

		// Copy the evalCtx, as dsp.Run() might change it.
		evalCtxCopy := *evalCtx
		// p is the physical plan, recv is the distsqlreceiver
		dsp.Run(ctx, planCtx, noTxn, p, recv, &evalCtxCopy, finishedSetupFn)()
		return resultRows.Err()
	}

	if err = ctxgroup.GoAndWait(ctx, execPlan, replanner); errors.Is(err, sql.ErrPlanChanged) {
		execCtx.ExecCfg().JobRegistry.MetricsStruct().Changefeed.(*Metrics).ReplanCount.Inc(1)
	}

	return err
}

func makePlan(
	execCtx sql.JobExecContext,
	jobID jobspb.JobID,
	details jobspb.ChangefeedDetails,
	initialHighWater hlc.Timestamp,
	checkpoint jobspb.ChangefeedProgress_Checkpoint,
	trackedSpans []roachpb.Span,
	selectClause string,
) func(context.Context, *sql.DistSQLPlanner) (*sql.PhysicalPlan, *sql.PlanningCtx, error) {

	return func(ctx context.Context, dsp *sql.DistSQLPlanner) (*sql.PhysicalPlan, *sql.PlanningCtx, error) {
		var blankTxn *kv.Txn

		planCtx := dsp.NewPlanningCtx(ctx, execCtx.ExtendedEvalContext(), nil /* planner */, blankTxn,
			sql.DistributionTypeAlways)

		var spanPartitions []sql.SpanPartition
		if details.SinkURI == `` {
			// Sinkless feeds get one ChangeAggregator on the gateway.
			spanPartitions = []sql.SpanPartition{{SQLInstanceID: dsp.GatewayID(), Spans: trackedSpans}}
		} else {
			// All other feeds get a ChangeAggregator local on the leaseholder.
			var err error
			spanPartitions, err = dsp.PartitionSpans(ctx, planCtx, trackedSpans)
			if err != nil {
				return nil, nil, err
			}
		}

		// Use the same checkpoint for all aggregators; each aggregator will only look at
		// spans that are assigned to it.
		// We could compute per-aggregator checkpoint, but that's probably an overkill.
		aggregatorCheckpoint := execinfrapb.ChangeAggregatorSpec_Checkpoint{
			Spans:     checkpoint.Spans,
			Timestamp: checkpoint.Timestamp,
		}

		var checkpointSpanGroup roachpb.SpanGroup
		checkpointSpanGroup.Add(checkpoint.Spans...)

		aggregatorSpecs := make([]*execinfrapb.ChangeAggregatorSpec, len(spanPartitions))
		for i, sp := range spanPartitions {
			watches := make([]execinfrapb.ChangeAggregatorSpec_Watch, len(sp.Spans))
			for watchIdx, nodeSpan := range sp.Spans {
				initialResolved := initialHighWater
				if checkpointSpanGroup.Encloses(nodeSpan) {
					initialResolved = checkpoint.Timestamp
				}
				watches[watchIdx] = execinfrapb.ChangeAggregatorSpec_Watch{
					Span:            nodeSpan,
					InitialResolved: initialResolved,
				}
			}

			aggregatorSpecs[i] = &execinfrapb.ChangeAggregatorSpec{
				Watches:    watches,
				Checkpoint: aggregatorCheckpoint,
				Feed:       details,
				UserProto:  execCtx.User().EncodeProto(),
				JobID:      jobID,
				Select:     execinfrapb.Expression{Expr: selectClause},
			}
		}

		// NB: This SpanFrontier processor depends on the set of tracked spans being
		// static. Currently there is no way for them to change after the changefeed
		// is created, even if it is paused and unpaused, but #28982 describes some
		// ways that this might happen in the future.
		changeFrontierSpec := execinfrapb.ChangeFrontierSpec{
			TrackedSpans: trackedSpans,
			Feed:         details,
			JobID:        jobID,
			UserProto:    execCtx.User().EncodeProto(),
		}

		cfKnobs := execCtx.ExecCfg().DistSQLSrv.TestingKnobs.Changefeed
		if knobs, ok := cfKnobs.(*TestingKnobs); ok && knobs != nil && knobs.OnDistflowSpec != nil {
			knobs.OnDistflowSpec(aggregatorSpecs, &changeFrontierSpec)
		}

		aggregatorCorePlacement := make([]physicalplan.ProcessorCorePlacement, len(spanPartitions))
		for i, sp := range spanPartitions {
			aggregatorCorePlacement[i].SQLInstanceID = sp.SQLInstanceID
			aggregatorCorePlacement[i].Core.ChangeAggregator = aggregatorSpecs[i]
		}

		p := planCtx.NewPhysicalPlan()
		p.AddNoInputStage(aggregatorCorePlacement, execinfrapb.PostProcessSpec{}, changefeedResultTypes, execinfrapb.Ordering{})
		p.AddSingleGroupStage(
			dsp.GatewayID(),
			execinfrapb.ProcessorCoreUnion{ChangeFrontier: &changeFrontierSpec},
			execinfrapb.PostProcessSpec{},
			changefeedResultTypes,
		)

		p.PlanToStreamColMap = []int{1, 2, 3}
		dsp.FinalizePlan(planCtx, p)

		return p, planCtx, nil
	}
}

// changefeedResultWriter implements the `sql.rowResultWriter` that sends
// the received rows back over the given channel.
type changefeedResultWriter struct {
	rowsCh       chan<- tree.Datums
	rowsAffected int
	err          error
}

func makeChangefeedResultWriter(rowsCh chan<- tree.Datums) *changefeedResultWriter {
	return &changefeedResultWriter{rowsCh: rowsCh}
}

func (w *changefeedResultWriter) AddRow(ctx context.Context, row tree.Datums) error {
	// Copy the row because it's not guaranteed to exist after this function
	// returns.
	row = append(tree.Datums(nil), row...)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case w.rowsCh <- row:
		return nil
	}
}
func (w *changefeedResultWriter) IncrementRowsAffected(ctx context.Context, n int) {
	w.rowsAffected += n
}
func (w *changefeedResultWriter) SetError(err error) {
	w.err = err
}
func (w *changefeedResultWriter) Err() error {
	return w.err
}
