---
documentVersion: 1
status: release-ready, verified 2026-08-25
date: 2026-08-25
---

# Scoped workspace discovery implementation plan

## Goal and completion rule

Implement Cloister's local `scan -> review -> explicit local apply` workflow
without adding VM side effects or changing config schema version 4.

This checklist records implementation and verification work. Planned v1 items
and the final gate passed on 2026-08-25. The work is release-ready and verified.
It is not a published release.

## Phase 1: Reusable project selection

Files:

- `internal/workspace/discover.go`
- `internal/workspace/discover_test.go`
- `internal/workspace/resolve.go`
- `internal/workspace/resolve_test.go`

Checklist:

- [x] Expose the existing canonical root and selector behavior as a reusable
  project selection function.
- [x] Keep activation and discovery on the same selector validation path.
- [x] Reject empty or escaping selectors, root selection, nested projects,
  unused project-specific ignore keys, and symlinked project roots.
- [x] Resolve a path to the most specific configured profile scope.
- [x] Reject no-match and equal-specificity ambiguity instead of selecting by
  map order.
- [x] Keep project session policy and collection activation behavior identical.

Verification:

- [x] `TestDiscoverBuildsWholeProjectSessionsWithMinimalIgnores`
- [x] `TestDiscoverGuestRootsAreCollisionSafe`
- [x] `TestDiscoverRejectsUnusedProjectIgnore`
- [x] `TestProjectSessionMatchesCollectionActivation`
- [x] `TestProjectSessionRejectsPathsOutsideTheSelectedSet`
- [x] `TestResolveProfileChoosesMostSpecificScope`
- [x] `TestResolveProfileNoMatchListsCandidates`
- [x] `TestResolveProfileCanonicalizesRelativeSymlinkPath`

## Phase 2: Generic source adapters

Files:

- `internal/workspace/source/source.go`
- `internal/workspace/source/source_test.go`

Checklist:

- [x] Define a generic source interface returning portable descriptors, policy,
  canonical local roots, approved roots, adapter identity, and metadata digest.
- [x] Implement the generic selector adapter through reusable workspace
  selection.
- [x] Implement the workspace-manifest adapter with strict bounded reads of only
  canonical catalog metadata and its optional local overlay.
- [x] Keep local project overlays shallow and deterministic.
- [x] Exclude worktree sets unless explicitly requested.
- [x] Support adapter-injected catalog overrides without documenting or
  persisting private environment details.
- [x] Reject malformed metadata, unknown versions, duplicate identifiers,
  duplicate or nested roots, symlink escapes, and unapproved external roots.
- [x] Sanitize catalog errors so they do not disclose host paths.

Verification:

- [x] `TestGenericSelectorUsesWorkspaceSelectionSemantics`
- [x] `TestManifestLoadsCanonicalProjectsInDeterministicOrder`
- [x] `TestManifestAppliesOptionalLocalProjectOverlay`
- [x] `TestManifestLocalOverlayCanPreserveCanonicalPath`
- [x] `TestOverlayProjectsShallowMergesNonZeroLocalFields`
- [x] `TestOverlayProjectsRejectsDuplicateLocalNames`
- [x] Catalog override precedence and injected override tests in
  `internal/workspace/source/source_test.go`
- [x] `TestManifestExcludesWorktreesUnlessExplicitlyRequested`
- [x] `TestManifestExplicitlyDiscoversLegacyWorktreeSetFromCanonicalCatalog`
- [x] `TestManifestRejectsMalformedJSONAndUnknownVersion`
- [x] `TestManifestRejectsUnsafeDuplicateAndNestedProjects`
- [x] `TestManifestRejectsSymlinkEscapeAndUnapprovedExternalRoot`
- [x] `TestManifestOpensOnlyAllowlistedMetadata`
- [x] `TestManifestFailsClosedWhenOptionalLocalMetadataCannotBeRead`

## Phase 3: Portable proposal schema v1

Files:

- `internal/workspace/scan/model.go`
- `internal/workspace/scan/model_test.go`

Checklist:

- [x] Define schema version 1 with source metadata, projects, findings,
  runtimes, commands, services, policy, exclusions, cloud readiness, and
  unanswered future questions.
- [x] Require all collection fields, including empty collections.
- [x] Require clean relative slash paths and valid project references.
- [x] Reject duplicate project identifiers and paths.
- [x] Restrict `cloudReadiness` to `local_only`.
- [x] Normalize collections for deterministic portable JSON.
- [x] Clone before normalization so marshaling never mutates caller-owned data.
- [x] Reject unknown schema versions.

Verification:

- [x] `TestProposalJSONIncludesPortableCloudFieldsAndStableCollections`
- [x] `TestValidateProposalRejectsIncompleteAndUnknownSchema`
- [x] `TestMarshalProposalDoesNotMutateCallerCollections`

## Phase 4: Bounded metadata scanner

Files:

- `internal/workspace/scan/scanner.go`
- `internal/workspace/scan/scanner_test.go`
- `internal/workspace/scan/classify_test.go`
- `internal/workspace/scan/compose.go`
- `internal/workspace/scan/compose_test.go`

Checklist:

- [x] Validate canonical source and project roots before walking.
- [x] Enforce per-project entry and reported-byte caps with typed limit errors.
- [x] Classify from metadata without opening ordinary files.
- [x] Never open secret-like, credential, certificate, or machine-local config
  candidates.
- [x] Default clearly named `.env`, `.envrc`, and local appsettings templates to
  source/include while keeping them metadata-only unless independently present
  on the safe manifest allowlist.
- [x] Default `.envrc`, `.envrc.local`, and equivalent machine-local direnv
  config to `secret_local_config`/review, prune generated `.direnv` state before
  descent, and keep filenames that merely contain `envrc` as source.
- [x] Read Compose service inventory from a bounded non-expanding YAML syntax
  tree. Reject aliases, merge keys, node count and depth overruns, duplicate
  top-level or service keys, and unexpected shapes. Extract only top-level
  `services` key names and never retain values.
- [x] Default all non-dump SQL to `database_script`/include, keep
  high-confidence backup and dump SQL at `database_dump`/exclude, and never
  manifest-parse SQL.
- [x] Open only allowlisted safe manifests and cap each parse at 1 MiB.
- [x] Record command names without script bodies or other manifest content.
- [x] Prune only clear dependency, cache, generated, repository metadata, and
  host-private agent state directories.
- [x] Prune case-insensitive `bin/Debug`, `bin/Release`, `obj/Debug`, and
  `obj/Release` subtrees without pruning arbitrary `bin` or `obj` source trees.
- [x] Keep ambiguous source, local config, SQL, repository instructions, and
  unknown large files available for review.
- [x] Reject symlinked project roots and do not follow nested symlinks.
- [x] Hide absolute paths and file contents in boundary errors.
- [x] Produce deterministic proposals.
- [x] Produce a bounded, deterministic per-source content fingerprint in the
  same scanner traversal as the proposal. Prefer `ScanWithSnapshot` so the
  initial scan does not walk twice. Keep `Scan` compatibility by discarding the
  snapshot.
- [x] Hash sorted project identity plus every visited entry's project-relative
  path, type and mode, reported size, and modification time. Include pruned
  directory entries. Do not read file contents.
- [x] Expose a bounded `ContentFingerprint` recheck path that reuses the same
  project validation, symlink behavior, prune rules, and entry and byte caps,
  without opening manifest contents.
- [x] Keep the fingerprint local-only. It must never enter portable proposal
  JSON.

Verification:

- [x] `TestScanClassifierMatrixAndSafeManifestInventory`
- [x] `TestScanNeverOpensSecretLikeFiles`
- [x] Safe config template classifier and metadata-only opener tests.
- [x] SQL migration, schema, generic query, backup, and dump classification
  tests.
- [x] Known .NET configuration subtree pruning tests that retain source under
  arbitrary `bin` and `obj` paths.
- [x] `TestScanRejectsProjectSymlinksAndDoesNotFollowNestedSymlinks`
- [x] `TestScanReturnsTypedEntryAndByteLimitErrors`
- [x] `TestScanResultsAreDeterministic`
- [x] Adapter acceptance and descriptor containment tests in
  `internal/workspace/scan/scanner_test.go`
- [x] Scanner error sanitization tests in
  `internal/workspace/scan/scanner_test.go`
- [x] `TestScanProposalOmitsPackageScriptBodies`
- [x] The full classification matrix in
  `internal/workspace/scan/classify_test.go`
- [x] `TestClassifyDirectoryPruningIsConservative`
- [x] `TestPolicyPrunePatternsMatchClassifierDecisions`
- [x] direnv classifier and injected-opener tests covering `.envrc`,
  `.envrc.local`, `.envrc.example`, `.direnvrc`, `.direnv`, and source names
  that merely contain `envrc`.
- [x] Compose parsing tests in `internal/workspace/scan/compose_test.go`
  covering a compact alias-expansion bomb, aliases in every position, merge
  keys, excessive nesting, excessive node count, duplicate `services` and
  service keys, malformed shapes, an ordinary Compose file, and sanitized
  errors that carry no source snippet.
- [x] `TestScanWithSnapshotAndContentFingerprintDetectMetadataDrift` covers
  added, removed, renamed, size-only, mtime-only, and newly added secret-path
  entries without opening file contents.

## Phase 5: Private local state format v1

Files:

- `internal/workspace/scan/state.go`
- `internal/workspace/scan/state_test.go`

Checklist:

- [x] Store canonical source root, project mappings, config fingerprint, source
  fingerprint, content fingerprint, proposal digest, reviewed status, and
  proposal in format v1.
- [x] Require and validate `contentFingerprint` as a local-only freshness field.
  It must not appear in portable proposal JSON.
- [x] Validate profile names, canonical directories, one-to-one mappings, and
  proposal correspondence.
- [x] Reject a reviewed state containing unresolved decisions.
- [x] Decode current JSON strictly and reject unknown fields.
- [x] Detect proposal tampering with a normalized digest.
- [x] Fail closed for newer versions and older versions without registered
  migrations.
- [x] Provide explicit state and proposal migration registries that do not
  rewrite files during load.
- [x] Save with private parent and file permissions.
- [x] Write, sync, close, and atomically replace state files.
- [x] Preserve caller-owned collections during save.

Verification:

- [x] `TestStateRoundTripPermissionsAndAtomicReplace`
- [x] `TestSaveStateDoesNotMutateCallerCollections`
- [x] `TestLoadStateRejectsUnknownVersionsAndMalformedInput`
- [x] `TestSaveStateRejectsUnsafeProfilesAndMappings`
- [x] `TestLoadStateRejectsMissingContentFingerprint`

## Phase 6: Sectioned CLI review and local apply

Files:

- `cmd/workspace.go`
- `cmd/workspace_test.go`

Checklist:

- [x] Register `workspace scan`, `workspace review`, `workspace show`, and
  `workspace apply`.
- [x] Allow scan by profile or contained project path.
- [x] Ensure JSON output contains only proposal schema v1.
- [x] Save local mappings and freshness fingerprints, including the scanner
  content fingerprint, outside the portable proposal.
- [x] Present review findings in stable safety-oriented sections.
- [x] Require every `review` decision to become include or exclude.
- [x] Accept explicit `ia`/`include-all` and `ea`/`exclude-all` only when a
  section has multiple unresolved findings, applying the decision to the
  current and remaining unresolved findings in that section.
- [x] Explain bulk options in the prompt and never carry a bulk decision into
  another section.
- [x] Require final review-save confirmation.
- [x] Revalidate config, source catalog, mappings, proposal digest, and a fresh
  bounded content fingerprint before review and apply. Reject stale project-tree
  metadata before any state or config write.
- [x] Pin applied selectors to exact reviewed project paths.
- [x] Generate exact project-specific ignores from excluded findings.
- [x] Reject external mappings during local apply.
- [x] Print a field-level workspace delta and require apply confirmation.
- [x] Preserve all unrelated config and config schema version 4.
- [x] Make cancellation, invalid input, and EOF write nothing.
- [x] Keep the CLI file independent of VM and lifecycle packages.

Verification:

- [x] `TestWorkspaceCommandContract`
- [x] `TestWorkspaceStatePathRejectsUnsafeProfile`
- [x] `TestReviewRequiresEveryDecisionAndFinalConfirmation`
- [x] Deterministic bulk include for agent config, bulk exclude for secrets,
  section isolation, invalid input, EOF, and cancellation-preserves-state
  tests.
- [x] `TestBuildAppliedWorkspaceUsesExactExclusions`
- [x] `TestWorkspaceApplyCancellationLeavesConfigUnchanged`
- [x] `TestSelectWorkspaceSourceUsesConfiguredRootForProfileAndNestedPath`
- [x] `TestValidateProjectMappingsRejectsExternalCollision`
- [x] `TestHumanProposalOutputSummarizesIncludedSource`
- [x] `TestLoadWorkspaceSourceDoesNotFallbackFromMalformedManifest`
- [x] `TestSaveAppliedConfigPreservesUnrelatedConfigurationAndPrintsFieldDelta`
- [x] Stale config, stale catalog, changed mapping, and proposal digest tests in
  `cmd/workspace_test.go`
- [x] `TestLoadFreshRejectsProjectTreeDriftBeforeReviewAndApply` covers added,
  removed, renamed, size-only, mtime-only, and newly added secret-path drift
  with no state or config write.
- [x] `TestReviewCancelLeavesPersistedStateUnchanged`
- [x] `TestWorkspaceScanJSONOmitsLocalRootsAndSentinelContents`
- [x] `TestWorkspaceCommandFileDoesNotImportLifecyclePackages`

## Phase 7: Existing workspace integration

Files:

- `cmd/broker_workspace.go`
- `cmd/broker_workspace_test.go`

Checklist:

- [x] Route an explicitly selected local project through the same workspace
  session policy used for collection activation.
- [x] Preserve existing single-project behavior.
- [x] Reject paths outside the configured selected set.
- [x] Keep scan, review, show, and apply free of VM lifecycle calls.

Verification:

- [x] `TestBrokerSessionSpecAtPathUsesWorkspacePolicyForOneProject`
- [x] `TestBrokerSessionSpecAtPathRejectsProjectOutsideWorkspace`
- [x] `TestBrokerSessionSpecAtPathKeepsSingleProjectBrokerBehavior`
- [x] Existing broker start, dependency failure, and conflict tests remain
  green.

## Phase 8: Public documentation and hygiene

Files:

- `docs/design/scoped-workspace-discovery.md`
- `docs/design/scoped-workspace-discovery-plan.md`

Checklist:

- [x] Document the three-stage contract and explicit confirmations.
- [x] Document adapters, scanner limits, proposal schema v1, and local state
  format v1, including the local-only `contentFingerprint` field.
- [x] Document secret-safe metadata scanning, allowlisted parsing, symlink
  containment, conservative pruning, and no VM side effects.
- [x] Document SQL and safe config template include defaults, metadata-only
  handling, high-confidence dump exclusion, and .NET configuration pruning.
- [x] Document explicit section-scoped bulk review commands and final save
  confirmation.
- [x] Document that the bounded content fingerprint uses sorted project identity
  plus entry path, type and mode, size, and modification time, includes pruned
  directory entries, reads no contents, stays out of portable JSON, and is
  recomputed before review and apply.
- [x] Document pinned selectors, exact project ignore keys, field-level delta,
  and unchanged config schema version 4.
- [x] Document stale scan, project-tree drift, tamper, cancellation, EOF, and
  external mapping recovery.
- [x] Describe the remote-container direction only as a future portability
  target.
- [x] Keep future questions in `unansweredCloudQuestions` and readiness at
  `local_only`.
- [x] Use neutral examples and public paths only.
- [x] Search all new or modified tracked files for forbidden names, private
  identifiers, private paths, and the em dash character.
- [x] Run any repository-provided documentation checker.
- [x] Run `git diff --check`.

## Final verification gate

Run from the repository root after all implementation and documentation changes
are present:

```bash
go test ./internal/workspace/...
go test ./cmd
make vet
make test
git diff --check
```

Then inspect the complete diff, including untracked files, and confirm:

- [x] Portable JSON has no local physical root or private fingerprint fields,
  including `contentFingerprint`.
- [x] Secret-like and local config sentinels were never passed to the injected
  file opener.
- [x] Safe templates and all SQL remain metadata-only, non-dump SQL defaults
  include, and dump SQL defaults exclude.
- [x] Bulk review affects only the current section and still requires final
  save confirmation.
- [x] Review and apply reject a stale content fingerprint before any state or
  config write, including added, removed, renamed, size-only, mtime-only, and
  newly private path drift.
- [x] Review and apply cancellation paths leave bytes on disk unchanged.
- [x] External mappings can be scanned but cannot be locally applied.
- [x] Config version remains 4 and unrelated fields survive apply.
- [x] No workspace discovery command imports or calls VM lifecycle code.
- [x] Public docs contain no private identifiers, forbidden product references,
  private environment names, or em dash characters.
- [x] No published release or shipping claim is made. Status is release-ready
  and verified only.

## Final verification record

Verified 2026-08-25. `make test`, `make vet`, `make build`, gofmt, and
`git diff --check` passed. Whole-feature review found no Critical or Important
defects. A security review finding was fixed and re-verified.

A real running 8-project collection completed scan, sectioned review, apply,
and conservative obsolete-session reconciliation. Exactly 8 desired sessions
were active. Configured exclusions were absent in the guest. Repository
instructions and SQL source were present. Recursive guest grep found 2,039
matching C# files. Host Git and `gh` proxying worked from a guest project.
Host file descriptors were 20,191 of 491,520 before and 21,327 of 491,520
after, about 4.34% of the limit, with no whole-tree mount exhaustion.
