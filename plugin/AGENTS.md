# plugin AGENTS.md

## Purpose

`VarroaSecurityRealm` — a Jenkins plugin (Maven/HPI, artifact `varroa-mite-auth`)
providing CJOC-style federated authentication for every Jenkins controller Varroa
provisions. Built into the Varroa images and delivered to each Jenkins pod via init
container (not installed from the update center). Also carries the activity-feed
emitters and the dashboard-identity banner.

## Ownership

Owns everything under `plugin/`: the `org.jenkinsci.plugins.varroamiteauth` package,
its Jelly/webapp resources, and its unit tests. Contract changes (env vars, JCasC symbol
shape, HTTP endpoint paths) must be coordinated with external consumers — the
`internal/mite/jenkinstoken.go` signer and `internal/bundle/` — in the same change, per
the root AGENTS.md greenfield rule.

## Local Contracts

- **Auth — two credential types**, both handled in `VarroaSecurityRealm.java`:
  1. `varroa_token` cookie (OIDC ID token from the BFF login) — `JWTValidator.java`
     validates it **online** against the issuer's JWKS.
  2. `Authorization: Bearer` header: `vk_*` API keys go to `ApiKeyValidator.java`, which
     calls the gateway verify endpoint (`VARROA_APIKEY_VERIFY_URL`/`VARROA_CA_PEM`,
     60s/10s pos/neg cache TTL, errors never cached). Non-`vk_` Bearer is the mite
     operator JWT — `JWTValidator.validateOperatorToken()` verifies RS256 **fully
     offline** via `VARROA_MITE_PUBKEY_PEM`, no Dex/network call — the counterpart to
     `MiteTokenSigner` in `internal/mite/jenkinstoken.go`. Operator JWTs without
     `varroa_typ` retain the system identities; `varroa_typ="user"` maps to the caller
     using its literal `groups` claim and never grants `ROLE:varroa:system-*`.
  The direct BOM-managed `mailer` plugin dependency supplies the reflective
  `Mailer.UserProperty` used when persisting non-empty user email claims.
  `VarroaCrumbExclusion.java` exempts both Bearer types from CSRF crumb checks.
  JCasC symbol `varroaMiteAuth` (fields `oidcIssuer`/`userClaimNames`/`groupClaimName`,
  falling back to `VARROA_OIDC_ISSUER`/`_USER_CLAIM`/`_GROUP_CLAIM`); round-trip fixtures
  `src/test/resources/.../realm-{minimal,explicit}.yml`.
- **`VarroaHeadlessFilter.java`** blocks the Jenkins web UI on a headless controller
  (`VARROA_HEADLESS=true`, read once into a volatile flag; a value change needs a pod
  restart). It registers with `PluginServletFilter.addFilter` from an `@Initializer`,
  the same pattern `HttpActivityFilter` uses. It denies with `503` before
  `InitMilestone.COMPLETED` and with `403` after `COMPLETED` for every caller except
  an exact `ROLE:varroa:system-mite`/`ROLE:varroa:system-operator` authority, an admin
  holding `Jenkins.ADMINISTER`, or a path on the fixed infrastructure allowlist
  (`/login`, `/crumbIssuer/`, `/wsagents/`, `/tcpSlaveAgentListener/`, `/jnlpJars/`,
  `/instance-identity/`, `/varroa-run`, agent JNLP descriptors, webhook paths, static
  assets, and a few more — the class holds the complete list). **The filter only
  denies.** It never grants a permission and never authenticates a request; the
  active authorization strategy still guards every allowlisted path. Any exception
  raised while evaluating a denial test counts as a denial, never a pass. An admin
  request that passes is audited by `VarroaBreakGlassAudit.java`
  (`controller.breakglass.session`, plus `controller.breakglass.script` on a
  sensitive path such as `/script` or `/manage`), both deduplicated per principal in a
  capped, self-clearing `ConcurrentHashMap`.
- **Activity emitters** (feed the push-based Activity page): `ActivitySink.java`
  (bounded, non-blocking, in-memory), `ActivityEvent.java` (normalized shape — actor,
  itemPath, buildNumber, result, url, message; `controller`/`namespace`/`phase`/`reason`
  always `""`, stamped downstream), `ActivityItemListener.java`
  (`item.created/updated/deleted/moved`), `ActivityRunListener.java` (`build.*`),
  `ActivitySaveableListener.java` (`config.changed`, **opt-in**,
  `VARROA_ACTIVITY_SAVEABLE=1`), `HttpActivityFilter.java` (last-HTTP-activity timestamp
  for hibernation idle gauges). `ActivityEndpoint.java` — `RootAction` at
  `/varroa-activity/events` (GET, requires `ROLE:varroa:system-mite`), drains
  `ActivitySink`, returns events + `idle` gauges. The same class's `doSchedules`
  serves `GET /varroa-activity/schedules`, reusing its method gate and
  `Items.getAllItems(Job.class)` walk: one row per job trigger, plus one `none`
  row for a job with no trigger, so the response is the cell's whole job
  inventory. `triggerType` maps from the trigger's simple class name
  (`TimerTrigger` → `cron`, `SCMTrigger` → `scm-poll`, `ReverseBuildTrigger` →
  `upstream`, `VarroaUpstreamTrigger` → `varroa-upstream`, else `other`); a
  `varroa-upstream` row carries `upstreamTenant`, `upstreamJob`, and
  `upstreamResult` (camelCase, this endpoint's own convention). An `upstream`
  row names several jobs and no tenant, so it carries the threshold in
  `upstreamResult`, an empty `upstreamTenant` and `upstreamJob`, and the
  resolved upstream job paths in its `spec` field, comma separated in
  configuration order; the resolution runs against the job's parent as the
  system identity, the way the trigger resolves its own projects, and drops a
  name that resolves to no job. Every other row omits all three upstream keys
  rather than sending them empty. A `cron` row
  carries the next three fire times,
  computed by mirroring `hudson.scheduler.CronTabList.create` — split the
  spec, skip the `TZ=` line, and seed each `CronTab` with
  `Hash.from(job.getFullName())`, the same hash Jenkins itself uses to fire —
  never derived in Go, which has no access to that hash seed. The walk stops
  adding rows past `MAX_SCHEDULE_ROWS` (5000) or `MAX_SCHEDULE_BYTES` (700
  KiB, whichever a `none`-heavy or a `cron`-heavy set hits first) and reports
  `truncated`, but keeps counting for an exact `total`; both bounds keep the
  set under the 1 MB NATS message limit the gateway republishes it inside.
- **Pipeline templates** (a consumer repository selects an operator-delivered pipeline
  template with a marker file; `add-pipeline-template-marker`): package
  `org.jenkinsci.plugins.varroamiteauth.template`, compile-scope `workflow-multibranch`,
  `workflow-cps` (also supplies `GlobalVariable`), `branch-api`, `scm-api`, and
  `snakeyaml-api`. `VarroaPipelineTemplateStore.java` (`GlobalConfiguration`, JCasC symbol
  `varroaPipelineTemplates`) holds the delivered template set; `resolve(folder, name,
  constraint)` picks the highest version matching a marker's constraint, scoped to the
  requesting job's tenant folder (a template resolves only when its own `folder` is empty
  or equals the requester's). `resolveCredentialScope` applies the same folder rule to
  `scmDefinition.credentialsId`: a tenant template resolves its credential in that tenant
  folder's own credential store only, an operator template (`folder` empty) may use the
  system store, and the lookup fails rather than widening to an ancestor store —
  `TemplateCredentialResolver.java` does this reflectively, because the credentials plugin
  is not a compile dependency here, the way `role-strategy` is not one for
  `VarroaTenantResolver`. `TemplateVersion.java` is the strict `MAJOR.MINOR.PATCH` parser
  and the four-constraint-form matcher. `VarroaMarkerValidator.java` is the Java subset
  checker and values validator mirroring `internal/bundle/pipelinetemplate` — not an
  extension point — plus the marker YAML parser (SnakeYAML's safe constructor, a 64 KiB
  cap before parsing); a shared golden corpus (mirrored under
  `src/test/resources/.../template/schema-corpus/` because the Maven build cannot see the
  Go module tree) proves the two sides agree. `VarroaTemplateValuesAction.java`
  (`InvisibleAction` + `RunAction2`) carries the resolved or rejected outcome on the `Run`;
  its `onAttached` callback releases `VarroaRunRecorder`'s held `STARTED` event the moment a
  flow definition attaches it. `VarroaTemplateParams.java` (`GlobalVariable`) exposes the
  values as `varroaParams`, never substituted into Groovy source text. `cloudbees-folder` is
  compile scope, not test: `WorkflowMultiBranchProject` extends branch-api's
  `MultiBranchProject`, which extends `ComputedFolder`, so any compile-time reference to the
  multibranch project type needs the whole chain resolvable.
  `VarroaMarkerFlowDefinition.java` (`FlowDefinition`) runs at branch build start: it reads
  the marker at the branch's trusted revision (`SCMSource.getTrustedRevision`, never the
  tip), resolves and validates it against the store, attaches a `VarroaTemplateValuesAction`,
  and builds the execution from the resolved template's `spec.jenkinsfile` or
  `spec.scmDefinition`. A read failure falls back from `SCMFileSystem` to a checkout in a
  temporary workspace, mirroring `workflow-multibranch`'s own `SCMBinder` fallback. It builds
  an `scmDefinition`'s `hudson.plugins.git.GitSCM` reflectively, the same reason
  `TemplateCredentialResolver` reaches the credentials plugin reflectively: git-plugin is a
  test-only dependency here, added solely to exercise this path with a real local
  repository. Any failure attaches a rejected `VarroaTemplateValuesAction`, fails the build,
  and emits activity `pipeline.marker.rejected`. `VarroaTemplateFlowDefinition.java`
  (`FlowDefinition`) is the items-declared form: `template`, `templateVersion`, and `values`
  come from the item's own `config.xml`, already validated at composition time, so it emits
  no activity event on a run-time resolution failure. Both flow definitions share step 6
  (building the execution) and the tenant-folder and schema-parsing helpers, all
  package-private statics on `VarroaMarkerFlowDefinition`.
  `VarroaTemplateBranchProjectFactory.java` (`AbstractWorkflowBranchProjectFactory`) tests
  only that the marker path exists during branch indexing and hands every branch a
  `VarroaMarkerFlowDefinition`.
- **Build event capture** (feeds the `varroa_builds` stream through the mite loopback;
  gated end to end by `VARROA_CLAIM_GATE`): compile-scope `workflow-api`, `workflow-job`,
  `pipeline-graph-analysis`, and `workflow-cps` (the last compile-scope only because
  `VarroaTemplateParams` needs its `GlobalVariable`; test-scope `workflow-basic-steps`
  exercises the pipeline graph in tests only). `VarroaRunRecorder.java` is an outer class
  with three static `@Extension` nested classes, because `RunListener` and
  `FlowExecutionListener` are abstract classes: `Runs` (`RunListener`) emits `STARTED`
  and `COMPLETED`; `Flows` (`FlowExecutionListener`) attaches and detaches `Graph` on a
  running pipeline; `Graph` (`GraphListener`) emits `STAGE_STARTED` on a labelled block
  start and `STAGE` on its end, with the stage status derived from node actions, not from
  a chunk-status call. A per-run state map (bounded 512 entries, keyed by `<job full
  name>#<build number>`) holds each run's identity and sequence counter, which an
  attach to a resumed build seeds from the wall clock; a daemon timer
  emits one `HEARTBEAT` event per live run every `VARROA_RUN_HEARTBEAT_SECONDS` (default
  60 s), the identity fields, the run's `summary` text when its pipeline set one, and the
  run's cached pending input set. The wire model is `BuildEvent.java`/`BuildStage.java`/
  `BuildChange.java`; size caps and an oversize ladder keep one serialized event under
  512 KiB, and never truncate an identifier field. `BuildEvent` fields 38 to 41
  (`template_name`, `template_version`, `marker_path`, `marker_revision`) come from the
  run's `template.VarroaTemplateValuesAction`, empty for a run with no template; a
  `HEARTBEAT` event never carries them. Field 46, `summary`, comes from the run's own
  `VarroaSummaryAction.java`, and a `HEARTBEAT` event carries it only when it is not
  empty. Field 47, `inputs`, holds the run's pending `input` steps: `VarroaRunInputs.java`
  reads them by class name, because `pipeline-input-step` is a test-scope dependency, and
  `Graph.onNewHead` schedules one deferred re-check per arriving node on a daemon timer,
  because the step attaches its action to the run after the node exists. The re-check
  emits `run.input` with the run's whole pending set, and only on a set that changed; the
  `HEARTBEAT` event carries the cached set alone. Fields 48 and 49, `test_report` and
  `test_cases` (`BuildTestReport.java`/`BuildTestCase.java`), ride the `COMPLETED` event
  alone. `VarroaTestReport.java` reads the junit result by method name, because `junit`
  is a test-scope dependency: it finds the action whose class hierarchy holds the simple
  name `AbstractTestResultAction`, then walks `getResult()`, `getSuites()`, and each
  suite's cases once. A result with no `getSuites()` produces no report and no case, and
  the counters stay. `Runs.onCompleted` writes every case to `.varroa/tests.json` in a
  temporary directory and archives it through `run.pickArtifactManager().archive(...)`
  **before** `ArtifactCollector.collect(run)`, so the report's blob key comes from the
  collected entry; a scan that finds no entry sends an empty key with the size and the
  case count. A failed archive logs a warning, sends no report pointer, and keeps the
  event. The event carries the failed cases, then the skipped cases, at most 500
  (`MAX_TEST_CASES`), every string cut at 512 bytes; the ladder drops the last case
  before any stage. Field 50, `replay_of`, names the source run of a
  `VarroaReplayCause`; only `startedEventBuilder` sets it, so only `STARTED` carries it,
  and `toJson()` writes no key for an empty value. `RunListener.onStarted` can fire before
  a Varroa template flow definition attaches that action, so `Runs` holds the `STARTED`
  event for such a run until the action attaches, in either state, or the run completes
  with none. `VarroaTemplateDefinitions.java` decides which run that is. It names the
  definitions by `FlowDefinition` class, because this package holds no compile dependency
  on `template`'s flow definitions.
- **Claim gate** (admits a queue item and claims a run id before dispatch, with no I/O
  under the Jenkins queue lock): `VarroaQueueDecisionHandler.java`
  (`Queue.QueueDecisionHandler`, stamps `VarroaIntentAction.java`), `IntentKey.java`
  (helper: origin and intent-key derivation, the params digest, the revision wait),
  `VarroaClaimDispatcher.java` (`QueueTaskDispatcher`, blocks the item until granted),
  `VarroaClaimQueue.java` (bounded work queue, four daemon threads, a retry ladder),
  `VarroaHomeCache.java` + `VarroaHomeRefresher.java` (the tenant home map, below),
  `DuplicateRegistry.java`, `ClaimRegistry.java`, and `VarroaDeliveryFilter.java` +
  `DeliveryRegistry.java` (records `X-Varroa-Delivery` for the claimer). Every class in
  this group is a no-op unless `VARROA_CLAIM_GATE=1` (stamped by the operator from
  `recordStore.enabled`). `VarroaIntentAction.actor` carries the calling identity a claim
  reports to the ledger: `VarroaRunTriggerAction` stamps a caller-supplied value on a
  cross-tenant trigger, and `VarroaQueueDecisionHandler` derives one from the scheduling
  authentication (`Jenkins.getAuthentication2().getName()`, `anonymous` and
  `ACL.SYSTEM2` normalized to empty) when the action carries none; `VarroaClaimQueue`
  sends `action.getActor()` as the claim's `actor` field. A `duplicate` decision
  cancels the item with one `INFO` line: it is the expected answer to a lost race or a
  redelivered command.
- **Intra-tenant trigger forwarding** (a job pair split over two tenant slots keeps
  working): `VarroaHomeCache.java` holds the newest answer of `GET /v1/homes`, keyed
  by tenant, with the slot this cell holds, the role, and the tenant's job-path to
  slot map. **The plugin computes no home**; it reads the map the recorder wrote and
  the operator delivered, so the plugin and the claim fence read one home for every
  job. Two fallbacks keep the two in step: an empty entry list is "no opinion" and
  forwards nothing, and a job with no entry in a present map resolves to slot 0. A
  job of a tenant this cell does not host is never forwarded.
  `VarroaHomeRefresher.java` (`AsyncPeriodicWork`) is the only caller of the refresh,
  every 30 s, overridden by `VARROA_HOMES_POLL_MS` and gated on `VARROA_CLAIM_GATE`;
  a failed refresh keeps the previous answer. **The forward runs in
  `VarroaClaimQueue`, never in the handler or the dispatcher**, because both hold the
  Jenkins queue lock and a forward is a network call. The claimer resolves the home
  before it posts the claim, forwards a non-home job over
  `POST /v1/cross-tenant/build` with `caller_job_path` the job's own full path,
  `target_tenant` the same tenant, `seq` `s1`, and `intent_key` the key it derived,
  then cancels the local item. A standby holds no slot, so it forwards every
  submission. A `vetoed`/`NOT_HOME` reply is the one veto the claimer does not
  cancel: it refreshes the cache and forwards, and the one-hour budget bounds a
  repeat. A failed forward joins the claim's own retry class.
  `VarroaQueueDecisionHandler` gains one refusal: a **waiting** `build` step for a
  non-home job, detected by an action whose class simple name is
  `BuildTriggerAction`, because `Queue.schedule2` refuses on the first `false` and
  `BuildTriggerStepExecution` aborts before its wait fork, which would fail a step
  that does not wait. The handler reads the cache from memory, hands a notice to its
  own single-thread executor (`AbortPolicy`, queue of 32, never the claim pool), and
  catches every throwable of the hand-off so no notice defect can admit the build; a
  hand-off that does not run logs the same line at `WARNING`. The notice thread writes
  the line to the caller run's own console, reached through the item's
  `Cause.UpstreamCause` and `FlowExecutionOwner`, and falls back to the system log.
  The five counters are `varroa_plugin_forwarded_{scm,cron,api,other}`, keyed by the
  intent key's scheme, and `varroa_plugin_forward_refused`. `pipeline-build-step`,
  `pipeline-input-step`, and `junit` are **test-scope** dependencies only: no main-scope
  class names a type of any of the three.
- **Cross-tenant pipeline API** (the `varroa` global variable; D1, D2, D11, D12):
  `VarroaGlobalVariable.java` compiles `varroa/Varroa.groovy` and
  `varroa/VarroaRunHandle.groovy` (plugin resources, not classpath `.class` files) into
  the pipeline's own `GroovyClassLoader` on first use — that shell's `loadClass` cannot
  find these resources on its own source path, so the source text is read via this
  plugin's own classloader and handed to `GroovyClassLoader.parseClass(String, String)`
  directly. `Varroa.groovy` exposes `build`, `lastSuccessful`, `lock`, `summary`, and the
  `lock.token` property (backed by `VarroaCrossTenantBridge.currentLockToken`, `@Whitelisted`
  for sandboxed pipelines); `VarroaRunHandle.groovy` is the returned run handle, plus
  `refresh()` (backed by `VarroaCrossTenantBridge.refresh`, since D1 lists no public status
  step) and `copyArtifact` (calls the `varroaArtifact` step). `VarroaBuildStep.java`
  (`varroaBuild`) and `VarroaLockStep.java`'s waiting phase (`varroaLock`) hold no CPS
  thread while waiting: both reschedule themselves on `jenkins.util.Timer` between polls,
  on the shared cadence in `VarroaPollPolicy.java`. `VarroaLastSuccessfulStep.java` and
  `VarroaArtifactStep.java` are synchronous steps (`SynchronousNonBlockingStepExecution`);
  `VarroaSummaryStep.java` uses `SynchronousStepExecution`, because its whole body is one
  local action attach on the run and one cache update.
  `VarroaArtifactStep` downloads a signed URL through a `MasterToSlaveFileCallable` on the
  agent's own workspace channel, so the artifact's bytes never transit the controller.
  `VarroaLockStep.java` records the acquired lock on `VarroaActiveLocksAction.java` (a
  transient, per-run stack, innermost lock last), and exposes the fence token as the
  environment variable `VARROA_LOCK_TOKEN` through an `EnvironmentExpander`. That stack
  is `transient`, so a resumed build restores an empty one: `Execution.onResume`
  re-pushes the held name and token before it reschedules the renewal, or
  `varroa.lock.token` reads null inside a block that holds a lock.
  `VarroaCallSeqAction.java` is the run-scoped `s<n>` sequence `varroaBuild` uses for its
  intent key. `MiteLoopbackClient.java` gained one method per cross-tenant kind, a
  `forCrossTenant()` factory reading `VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS` (default
  1000) and `VARROA_CROSSTENANT_READ_TIMEOUT_MS` (default 10000, warns once at or below
  the mite hop's 9500 ms default); `VarroaCrossTenantReply.java` parses a reply into a
  failure code or fields, mapping a transport failure to the same `UNAVAILABLE` code a
  structured reply carries. `Varroa.groovy` hands the lock step a closure of its own that
  calls the caller's body, never the caller's closure itself; a nested `varroa.lock` block
  resumes correctly only through that wrapper, the same shape docker-workflow uses for
  `inside`. `VarroaUpstreamTrigger.java` (`Trigger<Job<?, ?>>`, `@Symbol("varroaUpstream")`)
  declares a job's upstream producer (`tenant`, `job`, `result` defaulting to `SUCCESS` and
  otherwise restricted to `SUCCESS`/`UNSTABLE`/`FAILURE`) for both a declarative `triggers`
  block and the scripted `pipelineTriggers` step; it starts no timer and polls nothing —
  `ActivityEndpoint.doSchedules` reports the declaration, and the recorder's fan-out reads it
  from there. Compile-scope `workflow-step-api` supplies the four cross-tenant steps' base
  classes; test-scope `pipeline-model-definition` runs `VarroaUpstreamTrigger`'s declarative-
  syntax test through a real `pipeline { triggers { ... } }` block.
- **Build-event durability chain** — `BuildEventSink.java` (bounded, non-blocking queue;
  drops only a `HEARTBEAT` event when full) feeds `RunSpool.java`
  (`$JENKINS_HOME/varroa/spool/<run_id>/`, one file per event, never drops a `COMPLETED`
  file) feeds `BuildEventDispatcher.java` (one daemon thread, posts to the mite loopback,
  retries a failed post forever). The spool holds a second record variant for a
  run-state push, named with an `S` prefix so its sequence space never collides
  with a build event's; `Entry.runState` tells the two apart, and the dispatcher
  skips a run-state record because its own pusher replays it.
  `MiteLoopbackClient.java` and `LoopbackTokenFile.java`
  call `POST /v1/claim` and `POST /v1/events` against `http://127.0.0.1:9095`, bearer
  token cached from `/var/run/varroa/loopback-token` and re-read after a `401`.
  `postRunState` posts `POST /v1/run-state` instead as nine headers and a raw
  gzip body, because base64 in a JSON body would add a third to every byte. The mite
  retains each forwarded event until the gateway's JetStream publish acknowledgement, and
  replays after a reconnect. `Uuid7.java` mints every run id. `VarroaBuildEventLifecycle.java`
  starts `BuildEventDispatcher` from an `@Initializer(after = PLUGINS_STARTED)`, replaying
  every run's spool backlog found on disk (oldest run first) before the dispatcher's thread
  starts, and stops the dispatcher, the run-heartbeat timer, and `VarroaClaimQueue`'s
  executor from a `@Terminator`. An event that stays above the loopback's 1 MiB body limit
  even after the D11 oversize ladder reduces it to identity fields only is dropped, not
  posted: the plugin logs a `WARNING` with the run id and the size and counts
  `varroa.plugin.events.oversized`.
- **Log and artifact storage** (feeds the mite's log and tail loopback
  routes and the `run.artifacts`/`run.log` ingest kinds; D1, D6): compile-scope
  `workflow-api` (C2-owned entry in `plugin/pom.xml`), `workflow-job`, and
  `pipeline-graph-analysis`. `VarroaLogStorageFactory.java` is the `@Extension`
  implementing `LogStorageFactory`; it hands a Pipeline build a
  `VarroaLogStorage.java` (`LogStorage`) only when `VARROA_MITE_LOG_URL` is an
  absolute `http://127.0.0.1` URL and the build carries a Varroa run id,
  otherwise Jenkins keeps its own `FileLogStorage`. `VarroaLogStorage` builds
  every listener stream with `LogStorage#wrapWithRemoteAutoFlushingListener`
  and `#wrapWithAutoFlushingBuffer`, so every byte already passed the masking
  `TaskListenerDecorator` chain — a bound credential never reaches a chunk —
  before it reaches `LogChunkBuffer.java` (ordered chunks, a per-build cap,
  node-id splitting). `LogChunkSender.java` posts each build's chunks one at
  a time to the mite loopback with a jittered backoff. `LogReadCache.java`
  serves `overallLog`/`stepLog` reads back from disk. `TailEndpoint.java` is
  the `@Extension RootAction` at `/varroa-log/tail`, requiring
  `ROLE:varroa:system-mite`; its request body is `{"runId", "jobPath",
  "buildNumber", "action", "leaseSeconds"}` in camelCase, the shape the
  shipped mite (`cmd/mite/loopback.go`) sends — the endpoint reads `runId`,
  `action`, and `leaseSeconds` only. `TailEndpoint.doCounters` serves the
  same root action's second route, `GET /varroa-log/counters`, under the
  same authority: one JSON document with one integer per stable
  snake_case counter name, with no `_total` suffix.
  `PluginCounters.java` is the process-wide, thread-safe registry: it owns
  every counter directly (incremented at each drop, push, forward, and
  refusal site) except two it reads live from
  `VarroaRunRecorder.oversizedDroppedCount()` and
  `LogChunkBuffer.stuckQueueWarningCount()`, which already track their
  own lifetime totals. The class holds the complete name list. `BuildArtifact.java` and
  `LogSummary.java` are immutable value objects mirroring their proto
  messages; `ArtifactCollector.java` walks `Run#getArtifacts()` into
  `BuildArtifact` entries, called from C2's `VarroaRunRecorder.onCompleted`.
  `artifact-manager-s3` is never a compile or runtime dependency: the
  operator's rendered JCasC fragment configures it on a controller, and this
  plugin reads its object prefix reflectively. `plugin/pom.xml` carries
  `artifact-manager-s3` and `aws-global-configuration` at **test scope
  only**, pinned to the versions task 0.5's compatibility spike verified
  (`review-log.md` "Task 0.5"), so `ArtifactStorageFragmentCascTest` can
  round-trip the fragment through the real configurators.
- **Rehydrated changeset parsing** (feeds a rehydrated build's
  `currentBuild.previousBuild.changeSets`; design D1a): package
  `org.jenkinsci.plugins.varroamiteauth.rehydrate`, three classes, no new
  compile dependency (`hudson.scm.SCM`/`ChangeLogParser`/`ChangeLogSet` are
  jenkins-core). `VarroaRehydratedSCM` (`hudson.scm.SCM`) is the class the
  operator names in a rehydrated `build.xml`'s `<checkouts>` element;
  `createChangeLogParser()` returns `VarroaChangeLogParser`, and `checkout`
  always throws — a rehydrated run is already terminal and never checks out.
  `VarroaChangeLogParser` (`hudson.scm.ChangeLogParser`) reads the operator's
  own `varroa-changelog` document (one `entry` per commit: `commitId`,
  `author`, `authorEmail`, `timestamp`, `msg`) into a `VarroaChangeLogSet`; a
  missing changelog file parses as an empty set instead of failing the load.
  `VarroaChangeLogSet.Entry` resolves its author with `User.getById(id,
  false)`, falling back to `User.getUnknown()`, and its
  `getAffectedPaths()` always answers empty — the record store carries only
  an affected-path count, never the paths themselves, so the operator never
  has real paths to write into the document.
- **Run trigger endpoint** (the mite's local call for a manual build trigger, a cancel,
  the two input-approval commands, and a replay; D3, D4, D8, D10, D11, run-trigger-endpoint):
  `VarroaRunTriggerAction.java`, a protected
  `RootAction` at `/varroa-run`, requires `ROLE:varroa:system-mite`. `POST
  /varroa-run/trigger` reads `runId`, `intentKey`, `jobPath`, `parameters`, and
  `actor`; stamps a `VarroaIntentAction` carrying the caller's run identifier, intent
  key (origin `VARROA_INTENT`, so the claim gate reads the key straight off the
  action instead of deriving one), and actor unchanged, plus a `ParametersAction` built from the
  job's declared parameter definitions; an omitted value takes the declared
  default and a supplied name the job does not declare is dropped and reported
  back in `droppedParameters`. Before it schedules, the trigger and replay routes
  call `findBuildForRunId`: a build of the job that already carries the run id
  (newest first, at most 100 builds, stopping at a build scheduled more than five
  minutes before the UUIDv7 time) makes the route schedule nothing and answer `202`
  with the unchanged body. The queue fold reads queued items only, and the
  transport can redeliver a command after its build left the queue. `POST /varroa-run/cancel` reads `runId`, `jobPath`,
  and `buildNumber`; it resolves its target by run identifier, never by job path
  alone, because one job can hold several queued items and several running
  builds — a `buildNumber` greater than zero stops a running build whose
  `VarroaIntentAction` carries the same run id, and zero searches the queue
  instead. `POST /varroa-run/adopt-node` reads `name`, refuses a name holding a
  path separator or starting with a dot, answers `409` for a node the process
  already holds, and otherwise loads `nodes/<name>/config.xml` with
  `Jenkins.XSTREAM` and adds it. Jenkins reads `nodes/` at boot only, so a
  resumed build needs this route to find the agent it was running on.
  `POST /varroa-run/load-build` takes `{jobPath, buildNumber}` and reads
  `Job#getBuildByNumber`, which is the load: Jenkins loads builds on
  demand, and a job reload loads no build. The run's own load resumes
  its execution. The route then confirms the run holds a place on
  `FlowExecutionList` and takes one when it does not, reading the list
  first because a second registration logs a warning. It answers
  `{building, registered, finished}` and starts nothing itself. The same
  route with `probe` set reads that list and the job's already loaded
  builds, and loads nothing, which is how the mite makes a redelivered
  resume touch nothing; a probe answers `404` for an unknown job alone,
  and reports a build that ended in this process as `finished`.
  `POST /varroa-run/proceed-input` reads `runId`, `jobPath`, `buildNumber`,
  `inputId`, `parameters`, and `actingUser`; it resolves the build by run
  identifier and the input through the run's own input action, and it answers
  `400` for a body that does not decode and for a body that names no run, no
  input, or no acting name, `404` for no build carrying the run identifier and
  for an input id the run does not hold, `409` for a settled input, `403` for a
  submitter the acting user fails, and `202` with a `droppedParameters` list. A
  supplied name the step does not declare is dropped and named, a file
  parameter's supplied value is dropped and named, a name the request omits
  takes the step's declared default, and the step's submitter parameter takes
  the acting name last. `POST /varroa-run/abort-input` reads the same body
  without `parameters`, answers the same codes, and runs no permission check of
  its own: the input step's own abort path admits a caller holding the
  build-cancel permission or the step's own submitter identity. Both routes
  re-read the run's pending set after a `202`, before they write it.
  `POST /varroa-run/replay` reads `runId`, `intentKey`, `jobPath`,
  `sourceRunId`, `script`, `parameters`, and `actingUser`. It answers `400`
  for a body that does not decode and for a body that names no run, no source
  run, no job, no script, or no acting name. The source run is required,
  because the record's `replay_of` value comes from it. It answers `404` for no item and `409` for an
  item that is not a parameterized `WorkflowJob`. It answers `409` for a job
  that `VarroaTemplateDefinitions.isTemplateDefined` accepts, because a
  template owns its own script. `VarroaRunRecorder` reads the same class to
  hold a template run's `STARTED` event, so the two readers cannot disagree.
  Inside the acting user's own authentication it answers `403` when the user
  does not hold `ReplayAction.REPLAY` on the job. The check reads the job,
  because the source build can be gone from a disposable cell. It then
  schedules one build with the parameters action, the stamped
  `VarroaIntentAction`, a `CauseAction` holding a `VarroaUserCause` and a
  `VarroaReplayCause` naming `sourceRunId`, and a
  `VarroaReplayFlowFactoryAction` over the script, and it answers `202` with
  `droppedParameters`. The factory action is a `CpsFlowFactoryAction2`, which
  `CpsFlowDefinition` and `CpsScmFlowDefinition` both honour before they read
  or check out their own script. It always builds a sandboxed execution, and
  it takes the job's own durability hint. It implements no
  `Queue.QueueAction`, so the intent action's same-run-id guard still folds
  a redelivered command into one queue item. The build check runs before the
  permission check, because a repeat schedules nothing. The recorder writes
  `VarroaReplayCause`'s source run into `replay_of` on the `STARTED` event
  alone. `replay` holds one word, so the route needs no `@WebMethod`.
  **A route of more than one word needs `@WebMethod(name = "...")`.** Stapler
  derives the URL token from the method name and never writes a dash, so
  `doLoadBuild` serves `loadBuild` and the dashed name every caller uses answers
  `404`. A test posts to each `/varroa-run` route over HTTP, with the crumb
  issuer cleared, because the crumb filter answers `403` before Stapler routes
  and would hide an unmapped route.
- **The acting-user seam** (D10, run-input-approval): every mite-delivered command that
  acts as a person reads its user from the request body, never from the request
  authentication, and runs every Jenkins call after its resolution inside
  `ACL.as2(user.authentication())`. The request authenticates the mite, and the body
  names the person. `VarroaActingUser.java` is the one builder of that authentication: a
  Spring Security `UsernamePasswordAuthenticationToken` whose principal is the acting name
  and whose authorities are `authenticated` plus one per non-empty group. Jenkins bridges
  it to the acegi read the input step's `submitter` check performs, so a Varroa group name
  in `submitter` admits any acting user whose command carries that group. A persisted
  user's impersonation carries `authenticated` alone and would refuse every group-scoped
  check, so no route uses one. A blank name is refused, and the route answers `400`. The
  `email` reaches no authority and no check.
- **A resumed build attaches through `Flows.onResumed`.** `RunListener.onStarted`
  fires in the process that began a build, so a build the mesh moves to another
  JVM has no per-run state, no heartbeat, no stage events, and no sequence
  counter there. `Flows.onResumed` and `Flows.onRunning` share one `attach`,
  which rebuilds the state from the run's persisted `VarroaIntentAction` (the
  run id travels inside `build.xml`), starts the heartbeat, and **emits the
  first heartbeat at once** — the recorder clears the run's resume only on a
  heartbeat from the named instance received after it asked, and gives up six
  minutes later. It emits no second `STARTED`, and refuses a build that is not
  running. An attach that rebuilt the state also schedules the run's **first
  run-state push** (`RunStatePusher.scheduleForResumedRun`): the graph listener
  arms a window per flow node, and a build that resumes inside one long step
  fires none, so this process would otherwise replicate nothing for the whole
  step and a second loss would resume from the dead process's copy.
  `VarroaLogStorageFactory` needs no change: it resolves the run from
  the same persisted action.
- **Run-state replication** (the pipeline mesh's plugin half):
  `RunStatePusher.java` holds one instance per run, armed by
  `VarroaRunRecorder.Graph.onNewHead` and by `Flows`'s own resume attach, and
  removed by `Runs#onCompleted`. The
  listener only schedules: it runs on the flow's own thread, so it reads no
  file and posts nothing. The resume path names a `FlowExecution` instead of a
  node, because the resumed build names no new node; the durability gate reads
  that execution, and a resume that names none arms nothing. The first schedule after an idle period arms one
  `jenkins.util.Timer` task two seconds out, and the task then posts the whole
  current file set once, serially. The set holds `build.xml` and `program.dat`
  every time, each `workflow/<n>.xml` whose size or modification time changed,
  and `nodes/<name>/config.xml` for each node an executor step named. A build
  whose `FlowDurabilityHint` is below `MAX_SURVIVABILITY` pushes nothing and
  logs once: a lower level writes the flow program at a checkpoint, so a copied
  set can be stale by whole stages. `VARROA_MITE_RUN_STATE_URL` gates the push,
  with the rule `VarroaLogStorageFactory.parseActivationUrl` applies. A refusal
  reason decides what happens next: `state-cap` and `foreign-run` stop the
  build's pushes, `incomplete` and `malformed` drop the record and keep
  pushing, and `gateway-budget-full` waits the named pause. Every other
  answer, and every transport failure, **keeps** the spooled record for a
  later window: only an answer a retry cannot change may drop it. The
  replay runs before the generation increments, and a record is dropped
  only when the spool holds a newer generation of the same path: a
  counter that moved is not supersession. A window that cannot drain the
  spool posts nothing new, so a later generation never overtakes a kept
  record whose source file may already be gone; such a window arms one
  more, because a push is otherwise scheduled only by a later flow node.
  That re-arm doubles its delay to a one-minute cap, a window that drains
  resets it, and a record older than the recorder's fifteen-minute arm
  window is dropped. A window that **succeeds** arms a refresh 30 seconds
  out, on a flag of its own, because a long step fires no flow node and
  the build record and the flow program both carry that step: without it
  a run's stored state is as old as its longest step. The refresh
  schedules an ordinary window rather than pushing, so it and a flow
  node still coalesce. A stopped pusher arms none.
  **One run posts one window at a time.** The window clears `armed` when
  it starts, so a node or a refresh arms the next one while it still
  posts, and `jenkins.util.Timer` runs ten tasks at once: two windows
  would post one path under two generations together, and the gateway
  fences the pushing process, not the generation. A task that finds a
  window running re-schedules itself one window later and leaves `armed`
  set, so it stays the run's single armed window. `remove` **stops** the pusher rather than only
  dropping the map entry: a task already on the timer holds the pusher
  itself and would otherwise post after the run ended. It also clears
  the run's own run-state records, because a stopped pusher replays
  nothing. It clears those records alone, never the run's directory: the
  same directory holds the run's build events, including a `COMPLETED`
  event the dispatcher may not have sent yet. One monitor orders that
  delete against a window's own append, so a window that is already past
  its stopped test leaves no record behind. The monitor is never held
  across a post, so a run's completion never waits on the network.
  Every post carries `PUSHER_EPOCH`, the JVM start time, which names this
  Jenkins process to the gateway: the push generation counts inside one
  process and starts again after a restart. A flow
  node file's size and time marker is written after its own push
  succeeds, so a failed push never reads as a replicated file. A push
  failure of any kind leaves the build running and emits no build event.
- **Build identity and the agent fence** (runs every tenant build as its own service
  identity, and fences it to its own tenant's agents): `VarroaTenantResolver.java`
  holds the one shared rule — `tenantOf(Queue.Task)` resolves a task to its owning
  `hudson.model.Item` (the task itself, else `getOwnerTask()` when that is an `Item`
  and not the task) and takes its `getFullName()` first path segment as the tenant;
  `hostsTenant(String)` reads the cell's applied role-strategy document (reflective,
  role-strategy is a test-only dependency) and reports true when it carries an
  item-type role named `varroa:tenant:<team>`. `VarroaQueueItemAuthenticator`
  (`QueueItemAuthenticator`, published by `VarroaQueueItemAuthenticatorProvider` at
  `@Extension(ordinal = 1000)`, above core's ordinal 100 and the pinned
  `workflow-durable-task-step`'s `AuthenticationFromBuild` at ordinal 959) answers
  `svc-<team>` with authorities `authenticated` and `ROLE:varroa:tenant:<team>` for a
  resolved tenant, else `null`. `VarroaQueueTaskDispatcher`
  (`QueueTaskDispatcher.canTake`) blocks a build from a node that carries no reserved
  `varroa-tenant-<team>` label (the fence fails closed) or carries another tenant's
  label; it never parses the readability-only `<team>-` node-name prefix, and never
  overrides `canRun`. `VarroaSecurityRealm.VarroaUserDetailsService.loadUserByUsername`
  answers a `svc-<team>` identity the same way, with no persisted `User` record, so
  the boundary holds on every `User.impersonate2()` reload path. The plugin carries no
  `authorize-project` dependency: that plugin's global JCasC authenticator holds one
  user id, and its per-job strategy falls back to the anonymous principal with no
  error when a user record is absent, so it cannot serve many tenants.
- **Banner/identity** — `VarroaBannerDecorator.java` (`PageDecorator`; user id/name/email,
  `VARROA_BANNER_URL`), rendered by
  `resources/.../VarroaBannerDecorator/header.jelly` + `webapp/banner.js` (feature-detects
  the sticky redesigned Jenkins header, installs at z-index 1000). `resources/index.jelly`
  is the Manage Jenkins plugin description.
- **Pool confidential store** — `PoolConfidentialStoreTest` (test only, no
  production class). Every cell of a pool boots on one operator-minted
  `secrets/` directory, so the byte format of that directory is a hard
  contract: `DefaultConfidentialStore` answers `null` for a file whose magic
  trailer does not verify, and Jenkins then generates a fresh key with no
  error. The test boots `JenkinsRule` on a seeded `$JENKINS_HOME` through a
  `HomeLoader`, asserts that `JnlpAgentReceiver.SLAVE_SECRET.mac("x")` equals
  the value the Go writer recorded, forces a `hudson.util.Secret` round trip,
  and asserts that all four key files are byte-identical after boot. The
  fixture under `src/test/resources/.../confidentialstore/` is generated by
  `go run ./hack/gen-cell-secrets-fixture`; regenerate it whenever
  `internal/controller/cells/confidentialstore.go` changes.
- **Build** — `pom.xml`: parent `plugin:5.31`, `jenkins.version` `2.555.3` (Java-21-only
  LTS), packaging `hpi`. `hpi-plugin.version` pinned to `3.1814.v77d15159f9b_d` (parent's
  default can't classify Java-21 class-file version 65 — see `pom.xml:20-30`). **Build
  JDK must be 21.** `dependencyManagement` imports `bom-2.555.x` to align JCasC + its
  test harness with the core baseline.

## Work Guidance

- Changes to `JWTValidator.validateOperatorToken`/`VARROA_MITE_PUBKEY_PEM` must stay in
  lockstep with `internal/mite/jenkinstoken.go` (`MiteTokenSigner`) — same algorithm,
  same claims.
- Changes to `ActivityEvent`'s JSON shape or the `/varroa-activity/events` response need
  a matching update to whatever parses it downstream, in the same change.
- Keep `ActivitySaveableListener` opt-in — it fires on every item config save.

## Verification

```bash
mvn -f plugin/pom.xml -B package   # requires JDK 21; runs unit + JCasC round-trip tests
```
