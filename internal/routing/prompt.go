package routing

import (
	"fmt"
	"strings"
)

// GenerateModelRoster builds the markdown section that gets injected into the
// supervisor prompt, giving it visibility into available models and their capabilities.
// If allowedModels is non-empty, only models in that list are included.
func GenerateModelRoster(ps *ProfileStore, allowedModels []string) string {
	eligible := ps.EligibleFiltered(RoleWorker, allowedModels)
	if len(eligible) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("## Available Models for Workers\n\n")
	b.WriteString("When spawning workers with `oat work`, choose the best model for the task using `--model <model>`.\n")
	b.WriteString("If you omit `--model`, the system picks the highest-scoring eligible model automatically.\n\n")

	// Table header — COVERAGE column is inserted between Score and Context (Q11)
	// so operators can immediately see how many probes backed the score.
	b.WriteString("| Model | Score | Coverage | Context | Latency | Strengths | Weaknesses |\n")
	b.WriteString("|-------|-------|----------|---------|---------|-----------|------------|\n")

	for _, p := range eligible {
		strengths := summarizeStrengths(p)
		weaknesses := summarizeWeaknesses(p)
		context := formatContext(p)
		latency := formatLatency(p)
		coverage := formatCoverage(p)
		fmt.Fprintf(&b, "| `%s` | %d | %s | %s | %s | %s | %s |\n",
			p.ModelID, p.OverallScore, coverage, context, latency, strengths, weaknesses)
	}

	b.WriteString("\n### Model Selection Guidelines\n\n")
	b.WriteString("**You MUST distribute tasks across the available models.** Do not send all tasks to the highest-scoring model.\n\n")
	b.WriteString("Route tasks by complexity:\n\n")
	b.WriteString("- **Complex** (multi-file refactors, architecture, debugging, services with many dependencies) → highest-scoring model with reasoning controls\n")
	b.WriteString("- **Standard** (single-service implementation, adding tests, CLI commands) → second-tier models (score 90-96)\n")
	b.WriteString("- **Simple** (contracts, schemas, docs, config, single-file fixes) → any eligible model, prefer lower-scoring ones to save capacity\n")
	b.WriteString("- **Time-sensitive** (blocking other tasks, CI gates, critical path) → lowest-latency eligible model\n\n")
	b.WriteString("A good distribution for a typical wave: ~30-40% of tasks to the top model, remainder spread across others.\n")
	b.WriteString("If all tasks seem equally complex, round-robin across eligible models.\n")

	// Staleness footer: the roster is injected once at supervisor spawn and
	// does not auto-refresh when operators onboard new models mid-session. A
	// follow-up issue tracks live reload; until then the supervisor is told to
	// ask for a restart.
	b.WriteString("\n_Note: this roster is a snapshot at supervisor-spawn time. Ask the operator to restart this supervisor after onboarding new models via `oat model onboard`._\n")

	return b.String()
}

// GenerateOrchestratorModelRoster builds the roster for orchestrator-eligible models.
// Used when auto-selecting which model to run the orchestrator agents on.
func GenerateOrchestratorModelRoster(ps *ProfileStore) string {
	eligible := ps.Eligible(RoleOrchestrator)
	if len(eligible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Available Supervisor Models\n\n")
	b.WriteString("| Model | Score | Context | Multi-Turn | Tool Reliability |\n")
	b.WriteString("|-------|-------|---------|------------|------------------|\n")

	for _, p := range eligible {
		context := formatContext(p)
		fmt.Fprintf(&b, "| `%s` | %d | %s | %.0f%% | %.0f%% |\n",
			p.ModelID, p.OverallScore, context, p.MultiTurn*100, p.ToolReliability*100)
	}

	return b.String()
}

func summarizeStrengths(p *ModelProfile) string {
	var s []string
	fullyProbed := p.IsFullyProbed()

	if p.ToolReliability >= 1.0 && p.ShellReliability >= 1.0 && p.FileWriteReliability >= 1.0 {
		s = append(s, "all tools reliable")
	}
	// Only claim "strong error recovery" if actually measured (not defaulted to 1.0)
	if p.ShellRecovery >= 1.0 && fullyProbed {
		s = append(s, "strong error recovery")
	}
	if p.HasReasoningControls() {
		s = append(s, "reasoning: "+p.ReasoningControls)
	}
	if p.EffectiveContextClass == "large" {
		s = append(s, "large context window")
	}
	// Only claim "good at long tasks" if actually measured (not defaulted to 1.0)
	if p.LargeOutput >= 1.0 && p.MultiTurn >= 1.0 && fullyProbed {
		s = append(s, "good at long tasks")
	}

	if len(s) == 0 {
		s = append(s, "general purpose")
	}
	return strings.Join(s, "; ")
}

func summarizeWeaknesses(p *ModelProfile) string {
	var w []string

	// Only report weaknesses for capabilities that were actually measured.
	// A score of 0.0 on a minimum probe set means "not tested", not "incapable".
	fullyProbed := p.IsFullyProbed()

	if p.ShellRecovery < 0.8 && (fullyProbed || p.ShellRecovery > 0) {
		w = append(w, fmt.Sprintf("shell recovery %.0f%%", p.ShellRecovery*100))
	}
	if !p.HasReasoningControls() && p.ReasoningControls != "not_tested" {
		w = append(w, "no reasoning controls")
	}
	if p.EffectiveContextClass == "small" || p.EffectiveContextClass == "unknown" {
		w = append(w, "limited/unknown context")
	}
	if p.TokenReporting < 0.9 && p.TokenReporting > 0 {
		w = append(w, "partial token reporting")
	}

	// Flag minimum-probe-set profiles so operators know optimistic 1.0 defaults
	// were used for untested capabilities (P0-C). Appended LAST so it reads as a
	// qualifier on the rest of the row.
	if p.ProbeSet == "minimum" {
		w = append(w, "⚠ minimum probe set")
	}

	if len(w) == 0 {
		return "none notable"
	}
	return strings.Join(w, "; ")
}

func formatLatency(p *ModelProfile) string {
	if !p.HasLatencyData() {
		return "n/a"
	}
	if p.LatencyAvgMs >= 5000 {
		return fmt.Sprintf("~%.1fs (slow)", float64(p.LatencyAvgMs)/1000)
	}
	if p.LatencyAvgMs >= 2000 {
		return fmt.Sprintf("~%.1fs", float64(p.LatencyAvgMs)/1000)
	}
	return fmt.Sprintf("~%dms (fast)", p.LatencyAvgMs)
}

// formatCoverage renders the probe-count cell for the worker roster table.
// Shows "X/Y" when counts are available, "—" (em-dash) for legacy profiles
// that pre-date the probes_run/probes_passed fields.
func formatCoverage(p *ModelProfile) string {
	if p.ProbesRun <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d", p.ProbesPassed, p.ProbesRun)
}

func formatContext(p *ModelProfile) string {
	if p.MaxInputTokens > 0 {
		if p.MaxInputTokens >= 1_000_000 {
			return fmt.Sprintf("%.1fM", float64(p.MaxInputTokens)/1_000_000)
		}
		return fmt.Sprintf("%dK", p.MaxInputTokens/1_000)
	}
	return p.EffectiveContextClass
}
