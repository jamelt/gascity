package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
)

// End-to-end for the ../assets check-path feature: a formula authored with the
// documented "../assets/scripts/checks/<name>" form must (1) compile to the
// absolute path of the highest-priority layer that ships the script, and
// (2) that absolute path must pass the ralph check runner's trusted-roots gate
// via FormulaSearchPaths and actually execute. This is the chain the brittle
// ".gc/scripts/checks/..." references are migrating to.
func TestRunRalphCheckAcceptsResolvedAssetPathFromFormulaLayer(t *testing.T) {
	tmp := t.TempDir()
	packFormulas := filepath.Join(tmp, "pack", "formulas")
	packChecks := filepath.Join(tmp, "pack", "assets", "scripts", "checks")
	cityFormulas := filepath.Join(tmp, "city", "formulas")
	cityChecks := filepath.Join(tmp, "city", "assets", "scripts", "checks")
	for _, dir := range []string{packFormulas, packChecks, cityFormulas, cityChecks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	formulaText := `
formula = "review"

[[steps]]
id = "gate"
title = "Gate"
[steps.check.check]
mode = "exec"
path = "../assets/scripts/checks/review-approved.sh"
`
	if err := os.WriteFile(filepath.Join(packFormulas, "review.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	// Both layers ship the script; the city (highest-priority) copy must win and
	// be the one that runs.
	for _, dir := range []string{packChecks, cityChecks} {
		if err := os.WriteFile(filepath.Join(dir, "review-approved.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write check %s: %v", dir, err)
		}
	}

	searchPaths := []string{packFormulas, cityFormulas}
	f, err := formula.NewParser(searchPaths...).LoadByName("review")
	if err != nil {
		t.Fatalf("LoadByName(review): %v", err)
	}
	resolved := f.Steps[0].Ralph.Check.Path
	wantWinner := filepath.Join(cityChecks, "review-approved.sh")
	if resolved != wantWinner {
		t.Fatalf("resolved check path = %q, want city shadow %q", resolved, wantWinner)
	}

	// CityPath/StorePath are deliberately unrelated to the resolved script's
	// location: the gate must trust it via FormulaSearchPaths (the pack/layer
	// roots), not via the city/store roots.
	cityPath := filepath.Join(tmp, "runtime-city")
	storePath := filepath.Join(tmp, "runtime-store")
	for _, dir := range []string{cityPath, storePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-asset-resolved",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    resolved,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-asset-resolved", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: searchPaths,
	})
	if err != nil {
		t.Fatalf("runRalphCheck rejected the resolved asset path: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
	}
}

// A resolved asset path must still be rejected when its layer is NOT among the
// trusted FormulaSearchPaths — confirming the feature widens trust only to the
// formula layers actually in play, not to arbitrary absolute paths.
func TestRunRalphCheckRejectsAssetPathWhenLayerNotTrusted(t *testing.T) {
	tmp := t.TempDir()
	packChecks := filepath.Join(tmp, "pack", "assets", "scripts", "checks")
	if err := os.MkdirAll(packChecks, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script := filepath.Join(packChecks, "review-approved.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check: %v", err)
	}

	cityPath := filepath.Join(tmp, "city")
	storePath := filepath.Join(tmp, "store")
	for _, dir := range []string{cityPath, storePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-untrusted",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    script,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-untrusted", Type: "task"}

	// No FormulaSearchPaths → the pack dir is not a trusted root.
	_, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err == nil {
		t.Fatalf("expected rejection for asset path outside trusted roots")
	}
}

// An in-flight graph keeps the exact formula source that compiled it. A pack
// pin reload may move FormulaSearchPaths to a new immutable cache entry while
// that graph still carries an absolute check path under the old entry. The
// persisted source's sibling assets directory remains the narrow trust root
// for that graph; the cache as a whole must not become trusted.
func TestRunRalphCheckAcceptsPersistedFormulaSourceAfterSearchPathMoves(t *testing.T) {
	tmp := t.TempDir()
	gcHome := filepath.Join(tmp, "gc-home")
	t.Setenv("GC_HOME", gcHome)
	oldPack := filepath.Join(gcHome, "cache", "repos", "old-pin", "packs", "fab-pack")
	newPack := filepath.Join(gcHome, "cache", "repos", "new-pin", "packs", "fab-pack")
	oldFormula := filepath.Join(oldPack, "formulas", "fab-build.formula.toml")
	oldCheck := filepath.Join(oldPack, "assets", "scripts", "checks", "implement-done.sh")
	for _, dir := range []string{filepath.Dir(oldFormula), filepath.Dir(oldCheck), filepath.Join(newPack, "formulas")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(oldFormula, []byte("formula = \"fab-build\"\n"), 0o644); err != nil {
		t.Fatalf("write old formula source: %v", err)
	}
	if err := os.WriteFile(oldCheck, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write old check: %v", err)
	}

	cityPath := filepath.Join(tmp, "city")
	storePath := filepath.Join(tmp, "rig")
	for _, dir := range []string{cityPath, storePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "in-flight fab-build",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.FormulaSourceMetadataKey:   oldFormula,
		},
	})
	check := beads.Bead{
		ID:   "check-old-pin",
		Type: "task",
		Metadata: map[string]string{
			beadmeta.CheckPathMetadataKey:       oldCheck,
			beadmeta.CheckTimeoutMetadataKey:    "30s",
			beadmeta.RootBeadIDMetadataKey:      root.ID,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	}
	subject := beads.Bead{ID: "run-old-pin", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(newPack, "formulas")},
	})
	if err != nil {
		t.Fatalf("runRalphCheck rejected persisted old-pin asset: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
	}
	// The persisted source is the provenance anchor, not a permanent cache
	// trust grant. Once that source is removed, the old absolute asset must
	// fail closed even if the script itself still exists.
	if err := os.Remove(oldFormula); err != nil {
		t.Fatalf("remove old formula source: %v", err)
	}
	if _, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(newPack, "formulas")},
	}); err == nil {
		t.Fatal("deleted formula provenance continued to trust the old-pin asset")
	}
}

func TestRunRalphCheckPersistedFormulaSourceTrustsOnlySiblingAssets(t *testing.T) {
	tmp := t.TempDir()
	gcHome := filepath.Join(tmp, "gc-home")
	t.Setenv("GC_HOME", gcHome)
	pack := filepath.Join(gcHome, "cache", "repos", "old-pin", "packs", "fab-pack")
	formulaSource := filepath.Join(pack, "formulas", "fab-build.formula.toml")
	outsideAssetTree := filepath.Join(pack, "private", "check.sh")
	for _, dir := range []string{filepath.Dir(formulaSource), filepath.Dir(outsideAssetTree), filepath.Join(tmp, "city"), filepath.Join(tmp, "rig")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(formulaSource, []byte("formula = \"fab-build\"\n"), 0o644); err != nil {
		t.Fatalf("write formula source: %v", err)
	}
	if err := os.WriteFile(outsideAssetTree, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write unrelated script: %v", err)
	}
	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "in-flight fab-build",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.FormulaSourceMetadataKey:   formulaSource,
		},
	})
	check := beads.Bead{ID: "check-outside-assets", Type: "task", Metadata: map[string]string{
		beadmeta.CheckPathMetadataKey:  outsideAssetTree,
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}}
	subject := beads.Bead{ID: "run-outside-assets", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}

	if _, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath: filepath.Join(tmp, "city"), StorePath: filepath.Join(tmp, "rig"),
	}); err == nil {
		t.Fatal("persisted formula source trusted a script outside its sibling assets tree")
	}
}

func TestTrustedWorkflowFormulaSourceRejectsRootProvenanceOutsideRepoCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(tmp, "gc-home"))
	formulaSource := filepath.Join(tmp, "outside", "fab-pack", "formulas", "fab-build.formula.toml")
	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "untrusted workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.FormulaSourceMetadataKey:   formulaSource,
		},
	})
	child := beads.Bead{Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}

	if got := trustedWorkflowFormulaSource(store, child); got != "" {
		t.Fatalf("trustedWorkflowFormulaSource = %q, want empty for source outside the configured repo cache", got)
	}
}

func TestRunRalphCheckRejectsCheckLocalFormulaSourceAsTrustProvenance(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	pack := filepath.Join(tmp, "cache", "untrusted", "fab-pack")
	formulaSource := filepath.Join(pack, "formulas", "fab-build.formula.toml")
	checkPath := filepath.Join(pack, "assets", "scripts", "check.sh")
	for _, dir := range []string{filepath.Dir(formulaSource), filepath.Dir(checkPath), filepath.Join(tmp, "city"), filepath.Join(tmp, "rig")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(formulaSource, []byte("formula = \"fab-build\"\n"), 0o644); err != nil {
		t.Fatalf("write formula source: %v", err)
	}
	if err := os.WriteFile(checkPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check: %v", err)
	}
	store := beads.NewMemStore()
	check := beads.Bead{ID: "check-local-provenance", Type: "task", Metadata: map[string]string{
		beadmeta.CheckPathMetadataKey:     checkPath,
		beadmeta.FormulaSourceMetadataKey: formulaSource,
	}}
	subject := beads.Bead{ID: "run-local-provenance", Type: "task"}

	if _, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath: filepath.Join(tmp, "city"), StorePath: filepath.Join(tmp, "rig"),
	}); err == nil {
		t.Fatal("check-local formula source metadata granted absolute-path trust")
	}
}

// End-to-end for the templated ../assets check-path fix: an expansion formula
// authored with "../assets/scripts/checks/{target}.sh" is deferred at parse
// time, then resolved when runtime fan-out (CompileExpansionFragment)
// substitutes {target}. The materialized ralph control bead must carry the
// ABSOLUTE highest-priority layer asset path in gc.check_path — not the relative
// "../assets/..." form the runtime would resolve against the store/work dir and
// fail to find — and that absolute path must pass the ralph runner's
// trusted-roots gate and actually execute.
func TestRunRalphCheckAcceptsExpandedTemplatedAssetPath(t *testing.T) {
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	tmp := t.TempDir()
	packFormulas := filepath.Join(tmp, "pack", "formulas")
	packChecks := filepath.Join(tmp, "pack", "assets", "scripts", "checks")
	cityFormulas := filepath.Join(tmp, "city", "formulas")
	cityChecks := filepath.Join(tmp, "city", "assets", "scripts", "checks")
	for _, dir := range []string{packFormulas, packChecks, cityFormulas, cityChecks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	formulaText := `
formula = "expand-gate"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.gate"
title = "Gate"
[template.ralph]
max_attempts = 3
[template.ralph.check]
mode = "exec"
path = "../assets/scripts/checks/{target}.sh"
`
	if err := os.WriteFile(filepath.Join(packFormulas, "expand-gate.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	// {target} -> "build"; both layers ship build.sh, city (highest) must win.
	for _, dir := range []string{packChecks, cityChecks} {
		if err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write check %s: %v", dir, err)
		}
	}

	searchPaths := []string{packFormulas, cityFormulas}
	target := &formula.Step{ID: "build", Title: "Build"}
	fragment, err := formula.CompileExpansionFragment(context.Background(), "expand-gate", searchPaths, target, nil)
	if err != nil {
		t.Fatalf("CompileExpansionFragment(expand-gate): %v", err)
	}

	var resolved string
	for _, step := range fragment.Steps {
		if p := step.Metadata[beadmeta.CheckPathMetadataKey]; p != "" {
			resolved = p
			break
		}
	}
	if resolved == "" {
		t.Fatal("no materialized step carried a gc.check_path")
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("materialized gc.check_path = %q, want an absolute layer asset path", resolved)
	}
	if want := filepath.Join(cityChecks, "build.sh"); resolved != want {
		t.Fatalf("materialized gc.check_path = %q, want city shadow %q", resolved, want)
	}

	// The resolved absolute path must pass the runtime trusted-roots gate (via
	// FormulaSearchPaths) and actually execute. CityPath/StorePath are unrelated
	// to the script location on purpose.
	cityPath := filepath.Join(tmp, "runtime-city")
	storePath := filepath.Join(tmp, "runtime-store")
	for _, dir := range []string{cityPath, storePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-expanded-templated",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    resolved,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-expanded-templated", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: searchPaths,
	})
	if err != nil {
		t.Fatalf("runRalphCheck rejected the expanded templated asset path: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
	}
}

// Regression: a resolved ../assets check path must pass the trusted-roots gate
// even when the formula layer directory is NOT named "formulas" (e.g. a custom
// or absolute formulas_dir) and lives outside the city/store trees. The gate
// must trust the layer's sibling assets/ tree, not just a "formulas"-named
// layer's parent. Before the fix this failed with "escapes trusted roots".
func TestRunRalphCheckAcceptsResolvedAssetPathFromNonFormulasLayerName(t *testing.T) {
	tmp := t.TempDir()
	// Layer dir named "flows" (a custom formulas_dir), outside city/store.
	layerDir := filepath.Join(tmp, "ext", "flows")
	checksDir := filepath.Join(tmp, "ext", "assets", "scripts", "checks")
	for _, dir := range []string{layerDir, checksDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	formulaText := `
formula = "review"

[[steps]]
id = "gate"
title = "Gate"
[steps.check.check]
mode = "exec"
path = "../assets/scripts/checks/review-approved.sh"
`
	if err := os.WriteFile(filepath.Join(layerDir, "review.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checksDir, "review-approved.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check: %v", err)
	}

	searchPaths := []string{layerDir}
	f, err := formula.NewParser(searchPaths...).LoadByName("review")
	if err != nil {
		t.Fatalf("LoadByName(review): %v", err)
	}
	resolved := f.Steps[0].Ralph.Check.Path
	if want := filepath.Join(checksDir, "review-approved.sh"); resolved != want {
		t.Fatalf("resolved check path = %q, want %q", resolved, want)
	}

	cityPath := filepath.Join(tmp, "runtime-city")
	storePath := filepath.Join(tmp, "runtime-store")
	for _, dir := range []string{cityPath, storePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-nonformulas-layer",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    resolved,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-nonformulas-layer", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: searchPaths,
	})
	if err != nil {
		t.Fatalf("runRalphCheck rejected a non-\"formulas\" layer asset path: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
	}
}
