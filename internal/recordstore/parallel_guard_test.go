package recordstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Reasons a database test stays serial.
const (
	// serialReasonMeter: the test installs a global meter provider
	// through useTestMeterProvider.
	serialReasonMeter = "replaces the global meter provider"
	// serialReasonRole: the test checks or creates a cluster-wide
	// Postgres role, and its role-absent check must run before any
	// parallel test creates that role.
	serialReasonRole = "reads or creates a cluster-wide role"
)

// serialDatabaseTests lists every database test that must not call
// t.Parallel. Every other database test calls t.Parallel as its first
// statement. TestDatabaseTestsRunInParallel enforces both rules.
var serialDatabaseTests = map[string]string{
	"TestActivityConsumerCreatesMissingPartition":                           serialReasonMeter,
	"TestActivityConsumerDuplicateRedelivery":                               serialReasonMeter,
	"TestActivityConsumerExpiredEventDropped":                               serialReasonMeter,
	"TestActivityConsumerInsertsRow":                                        serialReasonMeter,
	"TestActivityConsumerUndecodablePayload":                                serialReasonMeter,
	"TestAfterMarkRetry_UnboundHomeSlotPublishesNothing":                    serialReasonMeter,
	"TestAnnounceHookMarksCellLostWithAFreshHeartbeat":                      serialReasonMeter,
	"TestClaimResponderStartMark":                                           serialReasonMeter,
	"TestConcurrencyCancelBoundedAtEight":                                   serialReasonMeter,
	"TestConcurrencyCancelPublishesForOlderRun":                             serialReasonMeter,
	"TestCrossTenantMetrics_RequestsAndGrantDeniedCountThroughDispatch":     serialReasonMeter,
	"TestEndMarkedLostIntentSkipsTheRetryOfAParameterizedIntent":            serialReasonMeter,
	"TestEndStaleMarkedIntentEndsALiveOwnerAfterTheAge":                     serialReasonMeter,
	"TestExpireUnclaimedRunsEndsAnAbandonedGrant":                           serialReasonMeter,
	"TestExpireUnclaimedRunsMarksAnAgedQueuedRun":                           serialReasonMeter,
	"TestFenceVetoMetricCountsByReason":                                     serialReasonMeter,
	"TestFireOnePublishesActionsEvenWhenActivityInsertFails":                serialReasonMeter,
	"TestFireOnePublishesWakeThenRetryAndWritesActivity":                    serialReasonMeter,
	"TestFleetLockMetrics_GaugesReportStoredRowCounts":                      serialReasonMeter,
	"TestHandleClaimLogsNothingOnGrant":                                     serialReasonMeter,
	"TestIngestConsumerAcknowledgesMalformedEnvelope":                       serialReasonMeter,
	"TestRefireAttemptBoundStopsTheSixthAttempt":                            serialReasonMeter,
	"TestRefireClearsTheMarkerOnAPublishFailure":                            serialReasonMeter,
	"TestRefirePermanentRejectionKeepsTheMarker":                            serialReasonMeter,
	"TestRefirePublishesAndMarks":                                           serialReasonMeter,
	"TestRefireRejectedTriggerClearsTheMarker":                              serialReasonMeter,
	"TestRefireUnreachableTriggerClearsTheMarker":                           serialReasonMeter,
	"TestRefireRefusalRules":                                                serialReasonMeter,
	"TestRunUpstreamFanout_ARedeliveredCompletionPublishesOnce":             serialReasonMeter,
	"TestRunUpstreamFanout_AddressesTheSubscriberHomeSlot":                  serialReasonMeter,
	"TestRunUpstreamFanout_BothKindsBuildOnce":                              serialReasonMeter,
	"TestRunUpstreamFanout_CapCoversBothSetsTogether":                       serialReasonMeter,
	"TestRunUpstreamFanout_ClusterResolutionExcludesOtherClusterSubscriber": serialReasonMeter,
	"TestRunUpstreamFanout_DeniedSubscriberSkipped":                         serialReasonMeter,
	"TestRunUpstreamFanout_DisabledSubscriberSkipped":                       serialReasonMeter,
	"TestRunUpstreamFanout_FiresAfterAHomeMove":                             serialReasonMeter,
	"TestRunUpstreamFanout_ForeignListedPathIsDroppedAndCountedOnce":        serialReasonMeter,
	"TestRunUpstreamFanout_FortySubscribersCapAt32":                         serialReasonMeter,
	"TestRunUpstreamFanout_NativeEdgeIgnoresTheRelativePath":                serialReasonMeter,
	"TestRunUpstreamFanout_NativeEdgeMatchesTheQualifiedPath":               serialReasonMeter,
	"TestRunUpstreamFanout_NativeEdgeReachesSlotOneHolder":                  serialReasonMeter,
	"TestRunUpstreamFanout_NativeThresholdBelowResultDoesNotFire":           serialReasonMeter,
	"TestRunUpstreamFanout_NativeThresholdMetFires":                         serialReasonMeter,
	"TestRunUpstreamFanout_OneDisabledDeclarationDoesNotHideTheOther":       serialReasonMeter,
	"TestRunUpstreamFanout_OneSlotTenantKeepsTheSlotZeroCell":               serialReasonMeter,
	"TestRunUpstreamFanout_PublishFailureCountsAndContinues":                serialReasonMeter,
	"TestRunUpstreamFanout_PublishesGrantedSubscriber":                      serialReasonMeter,
	"TestRunUpstreamFanout_TakeoverAddressesThePromotedCell":                serialReasonMeter,
	"TestRunUpstreamFanout_ThresholdBelowResultDoesNotFire":                 serialReasonMeter,
	"TestRunUpstreamFanout_UnboundHomeSlotCountsUnplaced":                   serialReasonMeter,
	"TestRunUpstreamFanout_UnplacedSubscriberSkipped":                       serialReasonMeter,
	"TestSweepEndsASilentRunOnALiveOwner":                                   serialReasonMeter,
	"TestSweepFleetLocks_MetricsCountedThroughMaintenanceRound":             serialReasonMeter,
	"TestSweepFleetLocks_NilNotifierStillDeletesRows":                       serialReasonMeter,
	"TestSweepInterruptedRunsMarksASilentRun":                               serialReasonMeter,
	"TestSweepInterruptedRunsSkipsOnPendingBacklog":                         serialReasonMeter,
	"TestSweepInterruptedRunsSkipsOnStaleConsumer":                          serialReasonMeter,
	"TestSweepMarksCellLostAndCountsIt":                                     serialReasonMeter,
	"TestSweepSkipsQueuedPlaceholderWithNoOwningInstance":                   serialReasonMeter,
	"TestTakeOverLostRunsArmsOnlyOnce":                                      serialReasonMeter,
	"TestTakeOverLostRunsArmsWithNoTarget":                                  serialReasonMeter,
	"TestTakeOverLostRunsPublishesNoLockNotification":                       serialReasonMeter,
	"TestTakeOverLostRunsSkipsExcludedInstance":                             serialReasonMeter,
	"TestVetoMarksAnAcceptedRun":                                            serialReasonMeter,
	"TestEnsureGrantsGatewayRole":                                           serialReasonRole,
	"TestEnsureGrantsIdempotent":                                            serialReasonRole,
}

// databaseEntryPoints are the helpers through which a test reaches the
// live database. A test that calls one of them, directly or through
// another function of this package, is a database test.
var databaseEntryPoints = []string{"testDatabases", "requireTestDSN"}

// TestDatabaseTestsRunInParallel parses the test files of this package.
// It fails when a database test outside serialDatabaseTests does not
// start with t.Parallel, when a listed test calls t.Parallel, when a
// test that reaches useTestMeterProvider is not serial, and when a
// listed name is not a database test.
func TestDatabaseTestsRunInParallel(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("list test files: %v", err)
	}
	fset := token.NewFileSet()
	bodies := map[string][]*ast.BlockStmt{}
	var tests []*ast.FuncDecl
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = append(bodies[fn.Name.Name], fn.Body)
			if fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && isTestingTFunc(fn) {
				tests = append(tests, fn)
			}
		}
	}

	reaches := reachability(bodies)
	databaseTests := map[string]bool{}
	for _, fn := range tests {
		name := fn.Name.Name
		parallel := startsWithParallel(fn)
		meter := reaches(name, "useTestMeterProvider")
		if !reaches(name, databaseEntryPoints...) {
			if meter && parallel {
				t.Errorf("%s replaces the global meter provider: remove its t.Parallel call", name)
			}
			continue
		}
		databaseTests[name] = true
		_, serial := serialDatabaseTests[name]
		switch {
		case serial && parallel:
			t.Errorf("%s is in serialDatabaseTests but calls t.Parallel", name)
		case !serial && !parallel:
			t.Errorf("%s uses the database: call t.Parallel as its first statement, or list it in serialDatabaseTests", name)
		}
		if !serial && meter {
			t.Errorf("%s replaces the global meter provider: list it in serialDatabaseTests", name)
		}
	}

	var stale []string
	for name := range serialDatabaseTests {
		if !databaseTests[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("serialDatabaseTests lists %s, which is not a database test of this package", name)
	}
}

// isTestingTFunc reports whether fn takes one *testing.T parameter.
func isTestingTFunc(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) != 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// startsWithParallel reports whether the first statement of fn is a call
// to t.Parallel on its own *testing.T parameter. Go starts a parallel
// test only after every serial test ends, so a statement before the call
// runs beside the serial tests.
func startsWithParallel(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) == 0 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Parallel" {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == fn.Type.Params.List[0].Names[0].Name
}

// reachability returns a function that reports whether the function
// named from calls one of targets, directly or through other functions
// in bodies. It matches calls by name, so a method and a function with
// the same name count as one.
func reachability(bodies map[string][]*ast.BlockStmt) func(from string, targets ...string) bool {
	callees := map[string]map[string]bool{}
	for name, blocks := range bodies {
		set := map[string]bool{}
		for _, block := range blocks {
			ast.Inspect(block, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					set[fun.Name] = true
				case *ast.SelectorExpr:
					set[fun.Sel.Name] = true
				}
				return true
			})
		}
		callees[name] = set
	}
	return func(from string, targets ...string) bool {
		want := map[string]bool{}
		for _, target := range targets {
			want[target] = true
		}
		seen := map[string]bool{from: true}
		queue := []string{from}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for callee := range callees[cur] {
				if want[callee] {
					return true
				}
				if !seen[callee] {
					seen[callee] = true
					queue = append(queue, callee)
				}
			}
		}
		return false
	}
}
