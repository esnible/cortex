package plugins

import (
	"log/slog"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// WarnPluginDirections emits a startup WARN for every configured plugin
// that declares a set of pipeline directions (Capabilities().Directions)
// not including the chain it was actually placed in — e.g. jwt-validation
// (inbound-only) configured under `outbound:`.
//
// Advisory, never fatal. No plugin enforces direction at runtime, so a
// misplaced plugin is a probable misconfiguration rather than a
// guaranteed one: it typically runs as dead code (a validator that never
// sees a token, a parser whose protocol never appears on that side).
// Failing the boot would break configurations that work today, so this
// only makes the condition visible in logs — matching the precedent set
// by config.WarnEmptyPipelines, which treats an open proxy the same way.
//
// Plugins that declare no Directions are unconstrained and never warn
// (see PluginCapabilities.Supports). Unknown plugin names are skipped
// silently: plugins.Build already fails on those with a much better
// error listing every registered name, and duplicating it here would
// double up the diagnostic.
//
// This function lives in authlib/plugins rather than beside
// WarnEmptyPipelines in authlib/config because it needs the plugin
// catalog: plugins already imports config, so the reverse would be an
// import cycle.
//
// Call this from each cmd entry point AFTER Validate succeeds and BEFORE
// the pipelines are built, alongside config.WarnEmptyPipelines. Pass
// slog.Default() unless you need a scoped logger.
func WarnPluginDirections(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	byName := make(map[string]pipeline.PluginCapabilities)
	for _, e := range Catalog() {
		byName[e.Name] = e.Capabilities
	}
	warnChain(cfg.Pipeline.Inbound.Plugins, pipeline.Inbound, byName, logger)
	warnChain(cfg.Pipeline.Outbound.Plugins, pipeline.Outbound, byName, logger)
}

// warnChain checks one chain's entries against their declared directions.
func warnChain(
	entries []config.PluginEntry,
	dir pipeline.Direction,
	byName map[string]pipeline.PluginCapabilities,
	logger *slog.Logger,
) {
	for i, e := range entries {
		caps, known := byName[e.Name]
		if !known {
			continue // Build reports unknown names with a better error.
		}
		if caps.Supports(dir) {
			continue
		}
		logger.Warn("plugin is configured in a pipeline direction it does not declare support for; "+
			"it will run but is likely misplaced",
			"plugin", e.Name,
			"configured_direction", dir.String(),
			"declared_directions", directionNames(caps.Directions),
			"position", i+1,
		)
	}
}

// directionNames renders a Direction slice as strings for log output.
// Keeps the WARN readable ("[inbound]") instead of printing the
// underlying enum ints.
func directionNames(ds []pipeline.Direction) []string {
	if len(ds) == 0 {
		return nil
	}
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}
	return out
}
