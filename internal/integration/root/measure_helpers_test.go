//go:build linux && rootintegration

package root_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func runSupervisorMeasurements(ctx context.Context, live *rundirectory.LiveRun,
	network *hostnetwork.Session, transferCount int,
) runsupervisor.Final {
	window, windowErr := throughput.NewSettings(os.Getenv(supervisorMeasureEnv), rootMeasureDuration)
	targets, resolverSet, targetErr := supervisorMeasureTargets(ctx, network, transferCount, os.Getenv(supervisorWorkDirEnv))
	if resolverSet != nil {
		defer closeRunSupervisorResolverSet(resolverSet)
	}
	var measurements []throughput.Observation
	var measuring *childprocesses.ProcessSet
	var measureErr error
	if windowErr == nil && targetErr == nil {
		measurements, measuring, measureErr = throughput.New().ObserveNamespaces(ctx, childprocesses.New(),
			network, targets, window)
	}
	if measureErr == nil {
		_, measureErr = throughput.RelativeWeights(measurements, false)
	}
	final := runsupervisor.Final{Transfers: make([]runsupervisor.TransferResult, len(measurements))}
	for index, measurement := range measurements {
		exitCode := 0
		if measurement.Err() != nil {
			exitCode = 1
		}
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: measurement.Target().Transfer(), ExitCode: exitCode}
	}
	if measuring != nil {
		retainErr := live.RetainLivenessUntil(measuring.ContainmentDone())
		return internalRunSupervisorFinal(errors.Join(targetErr, windowErr, measureErr, retainErr))
	}
	return finishRunSupervisorOwners(live, network, final, errors.Join(targetErr, windowErr, measureErr))
}

func supervisorMeasureTargets(ctx context.Context, network *hostnetwork.Session, count int,
	workDir string,
) ([]throughput.NamespaceExecution, *namespaceresolvers.NamespaceResolverSet, error) {
	resolverSet, err := openRunSupervisorResolverSet(ctx, network, count)
	if err != nil {
		return nil, nil, err
	}
	targets := make([]throughput.NamespaceExecution, count)
	for index := range targets {
		id, _ := transfernumber.New(index + 1)
		resolver, accessErr := resolverSet.Access(id)
		if accessErr != nil {
			closeRunSupervisorResolverSet(resolverSet)
			return nil, nil, accessErr
		}
		targets[index], err = throughput.NewNamespaceExecution(id, workDir, resolver,
			supervisorChildEnvironment())
		if err != nil {
			closeRunSupervisorResolverSet(resolverSet)
			return nil, nil, err
		}
	}
	return targets, resolverSet, nil
}

func openRunSupervisorResolverSet(ctx context.Context, network *hostnetwork.Session,
	count int,
) (*namespaceresolvers.NamespaceResolverSet, error) {
	transfers := make([]transfernumber.Number, count)
	for index := range transfers {
		transfers[index], _ = transfernumber.New(index + 1)
	}
	intent, err := namespaceresolvers.DirectIntent([]netip.Addr{netip.MustParseAddr("192.0.2.53")})
	if err != nil {
		return nil, err
	}
	return namespaceresolvers.Open(ctx, network, transfers, intent)
}

func closeRunSupervisorResolverSet(resolvers *namespaceresolvers.NamespaceResolverSet) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = resolvers.Close(ctx)
}
