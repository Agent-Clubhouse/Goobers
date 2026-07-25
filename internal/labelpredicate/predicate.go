// Package labelpredicate compiles the restricted CEL surface used to select
// backlog items by label.
package labelpredicate

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// Predicate combines additive CEL selection with the legacy required and
// excluded label lists.
type Predicate struct {
	required   []string
	excluded   []string
	referenced map[string]struct{}
	program    cel.Program
}

// Compile validates and compiles expression. The CEL surface is deliberately
// limited to string membership checks against labels, joined by &&, ||, and !.
func Compile(expression string, required, excluded []string) (*Predicate, error) {
	predicate := &Predicate{
		required:   append([]string(nil), required...),
		excluded:   append([]string(nil), excluded...),
		referenced: make(map[string]struct{}, len(required)),
	}
	for _, label := range required {
		predicate.referenced[label] = struct{}{}
	}
	if expression == "" {
		return predicate, nil
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, fmt.Errorf("CEL expression must not be blank")
	}

	env, err := cel.NewEnv(cel.Variable("labels", cel.MapType(cel.StringType, cel.BoolType)))
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}
	ast, issues := env.Compile(expression)
	if err := issues.Err(); err != nil {
		return nil, fmt.Errorf("compile CEL expression: %w", err)
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("CEL expression must return bool, got %s", ast.OutputType())
	}
	if err := validateExpression(ast.Expr(), predicate.referenced); err != nil {
		return nil, err
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build CEL program: %w", err)
	}
	predicate.program = program
	return predicate, nil
}

// RequiredLabels returns the legacy AND terms that providers may safely apply
// as a native query optimization.
func (p *Predicate) RequiredLabels() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.required...)
}

// ReferencesLabel reports whether the predicate's CEL or required-label input
// mentions label.
func (p *Predicate) ReferencesLabel(label string) bool {
	if p == nil {
		return false
	}
	_, ok := p.referenced[label]
	return ok
}

// Matches evaluates the exact predicate against labels.
func (p *Predicate) Matches(labels []string) (bool, error) {
	if p == nil {
		return true, nil
	}
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		set[label] = true
	}
	for _, label := range p.required {
		if !set[label] {
			return false, nil
		}
	}
	for _, label := range p.excluded {
		if set[label] {
			return false, nil
		}
	}
	if p.program == nil {
		return true, nil
	}
	value, _, err := p.program.Eval(map[string]any{"labels": set})
	if err != nil {
		return false, fmt.Errorf("evaluate CEL expression: %w", err)
	}
	matched, ok := value.Value().(bool)
	if !ok {
		return false, fmt.Errorf("evaluate CEL expression: got %T, want bool", value.Value())
	}
	return matched, nil
}

func validateExpression(expr *exprpb.Expr, referenced map[string]struct{}) error {
	call := expr.GetCallExpr()
	if call == nil || call.Target != nil {
		return unsupportedExpressionError()
	}
	switch call.Function {
	case operators.LogicalAnd, operators.LogicalOr:
		if len(call.Args) != 2 {
			return unsupportedExpressionError()
		}
		for _, arg := range call.Args {
			if err := validateExpression(arg, referenced); err != nil {
				return err
			}
		}
		return nil
	case operators.LogicalNot:
		if len(call.Args) != 1 {
			return unsupportedExpressionError()
		}
		return validateExpression(call.Args[0], referenced)
	case operators.In, operators.OldIn:
		if len(call.Args) != 2 || call.Args[1].GetIdentExpr().GetName() != "labels" {
			return unsupportedExpressionError()
		}
		constant := call.Args[0].GetConstExpr()
		if constant == nil {
			return unsupportedExpressionError()
		}
		if _, ok := constant.ConstantKind.(*exprpb.Constant_StringValue); !ok {
			return unsupportedExpressionError()
		}
		referenced[constant.GetStringValue()] = struct{}{}
		return nil
	default:
		return unsupportedExpressionError()
	}
}

func unsupportedExpressionError() error {
	return fmt.Errorf(`unsupported CEL expression: use only string membership checks ("label" in labels) combined with &&, ||, and !`)
}
