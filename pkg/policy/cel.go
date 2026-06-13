package policy

import (
	"fmt"

	"github.com/coding-workspace/simple-mitigation-1/pkg/features"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// celEnv builds the CEL environment used by all rules in a Document. Each
// FeatureVector field is declared as a top-level identifier so authors can
// write expressions like
//
//	k_temporal > 0.3 || k_spatial > 0.3
//
// without referencing a wrapper object. Two helper functions are also
// registered:
//
//	band(score, lo, hi) string  -> "up" if score >= hi, "down" if score <= lo, else "stable"
//	count_at_least(list, threshold) int  -> count of elements >= threshold
//
// Both helpers are stateless so they're safe to evaluate every tick without
// per-(rule,target,pod) caching.
func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("target", cel.StringType),
		cel.Variable("pod", cel.StringType),
		cel.Variable("p50_now", cel.DoubleType),
		cel.Variable("tail_now", cel.DoubleType),
		cel.Variable("k_spatial", cel.DoubleType),
		cel.Variable("accel_spatial", cel.DoubleType),
		cel.Variable("p50_max_horizon_ms", cel.IntType),
		cel.Variable("persistence_h", cel.IntType),
		cel.Variable("k_temporal", cel.DoubleType),
		cel.Variable("accel_temporal", cel.DoubleType),
		cel.Variable("variance", cel.DoubleType),
		cel.Variable("duration_above_hi_ms", cel.IntType),
		cel.Variable("window_size", cel.IntType),
		cel.Variable("has_spatial", cel.BoolType),
		cel.Variable("model_version", cel.StringType),
		cel.Variable("source_kind", cel.StringType),
		cel.Variable("p50_h", cel.ListType(cel.DoubleType)),
		cel.Variable("tail_h", cel.ListType(cel.DoubleType)),
		cel.Variable("horizon_ms", cel.ListType(cel.IntType)),
		cel.Function("band",
			cel.Overload("band_double_double_double",
				[]*cel.Type{cel.DoubleType, cel.DoubleType, cel.DoubleType},
				cel.StringType,
				cel.FunctionBinding(bandFn),
			),
		),
		cel.Function("count_at_least",
			cel.Overload("count_at_least_list_double",
				[]*cel.Type{cel.ListType(cel.DoubleType), cel.DoubleType},
				cel.IntType,
				cel.FunctionBinding(countAtLeastFn),
			),
		),
	)
}

func bandFn(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("band: want 3 args, got %d", len(args))
	}
	score, ok := args[0].Value().(float64)
	if !ok {
		return types.NewErr("band: arg0 not double")
	}
	lo, ok := args[1].Value().(float64)
	if !ok {
		return types.NewErr("band: arg1 not double")
	}
	hi, ok := args[2].Value().(float64)
	if !ok {
		return types.NewErr("band: arg2 not double")
	}
	switch {
	case score >= hi:
		return types.String("up")
	case score <= lo:
		return types.String("down")
	default:
		return types.String("stable")
	}
}

func countAtLeastFn(args ...ref.Val) ref.Val {
	if len(args) != 2 {
		return types.NewErr("count_at_least: want 2 args, got %d", len(args))
	}
	threshold, ok := args[1].Value().(float64)
	if !ok {
		return types.NewErr("count_at_least: threshold not double")
	}
	// CEL lists implement traits.Lister; iterate via that interface so the
	// helper works whether the backing slice is []float64, []any, or a
	// traits.Lister built from a list literal.
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("count_at_least: arg0 not list-like")
	}
	var c int64
	it := lister.Iterator()
	for it.HasNext() == types.True {
		v := it.Next()
		if f, ok := toFloat(v.Value()); ok && f >= threshold {
			c++
		}
	}
	return types.Int(c)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	default:
		return 0, false
	}
}

// vectorToActivation flattens a FeatureVector into the map[string]any CEL
// reads at evaluation time. Field names match the cel.Variable declarations
// in celEnv exactly.
func vectorToActivation(fv features.FeatureVector) map[string]any {
	return map[string]any{
		"target":                fv.Target,
		"pod":                   fv.Pod,
		"p50_now":               fv.P50Now,
		"tail_now":              fv.TailNow,
		"k_spatial":             fv.KSpatial,
		"accel_spatial":         fv.AccelSpatial,
		"p50_max_horizon_ms":    fv.P50MaxHorizonMs,
		"persistence_h":         int64(fv.PersistenceH),
		"k_temporal":            fv.KTemporal,
		"accel_temporal":        fv.AccelTemporal,
		"variance":              fv.Variance,
		"duration_above_hi_ms":  fv.DurationAboveHiMs,
		"window_size":           int64(fv.WindowSize),
		"has_spatial":           fv.HasSpatial,
		"model_version":         fv.ModelVer,
		"source_kind":           fv.SourceKind,
		"p50_h":                 fv.P50H,
		"tail_h":                fv.TailH,
		"horizon_ms":            fv.HorizonMs,
	}
}

// compileRule turns a single rule's `when` expression into a runnable
// cel.Program. Returns a wrapped error pointing at the rule name so a
// rule-author typo shows up clearly in the reload log.
func compileRule(env *cel.Env, r RuleSpec) (cel.Program, error) {
	ast, issues := env.Compile(r.When)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("policy: rule %q: compile %q: %w", r.Name, r.When, issues.Err())
	}
	prog, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("policy: rule %q: program: %w", r.Name, err)
	}
	return prog, nil
}
