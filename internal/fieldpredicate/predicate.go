// Package fieldpredicate compiles the restricted CEL and ordering surfaces used
// to select backlog items by provider-native scalar fields.
package fieldpredicate

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// Fields is a validated projection of provider-native scalar fields.
type Fields map[string]any

// Predicate is a compiled boolean expression over provider-native fields.
type Predicate struct {
	referenced map[string]struct{}
	program    cel.Program
}

// Compile validates and compiles expression. Field access is limited to
// fields["name"] comparisons with scalar constants, joined by &&, ||, and !.
func Compile(expression string) (*Predicate, error) {
	predicate := &Predicate{referenced: map[string]struct{}{}}
	if expression == "" {
		return predicate, nil
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, fmt.Errorf("CEL expression must not be blank")
	}

	env, err := cel.NewEnv(cel.Variable("fields", cel.MapType(cel.StringType, cel.DynType)))
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
	checked, err := cel.AstToCheckedExpr(ast)
	if err != nil {
		return nil, fmt.Errorf("convert checked CEL expression: %w", err)
	}
	if err := validateExpression(checked.GetExpr(), predicate.referenced); err != nil {
		return nil, err
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build CEL program: %w", err)
	}
	predicate.program = program
	return predicate, nil
}

// Matches evaluates the predicate. Every referenced field must be available
// and contain a supported scalar, even when CEL short-circuiting would skip it.
func (p *Predicate) Matches(fields Fields) (bool, error) {
	if p == nil || p.program == nil {
		return true, nil
	}
	names := make([]string, 0, len(p.referenced))
	for name := range p.referenced {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value, ok := fields[name]
		if !ok || value == nil {
			return false, fmt.Errorf("field %q is unavailable", name)
		}
		if _, err := scalarKind(value); err != nil {
			return false, fmt.Errorf("field %q: %w", name, err)
		}
	}
	value, _, err := p.program.Eval(map[string]any{"fields": map[string]any(fields)})
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
	case operators.Equals, operators.NotEquals,
		operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
		if len(call.Args) != 2 {
			return unsupportedExpressionError()
		}
		name, fieldOnLeft, ok := fieldComparison(call.Args[0], call.Args[1])
		if !ok {
			return unsupportedExpressionError()
		}
		if !fieldOnLeft && call.Function != operators.Equals && call.Function != operators.NotEquals {
			return unsupportedExpressionError()
		}
		referenced[name] = struct{}{}
		return nil
	default:
		return unsupportedExpressionError()
	}
}

func fieldComparison(left, right *exprpb.Expr) (string, bool, bool) {
	if name, ok := fieldAccess(left); ok && scalarConstant(right) {
		return name, true, true
	}
	if name, ok := fieldAccess(right); ok && scalarConstant(left) {
		return name, false, true
	}
	return "", false, false
}

func fieldAccess(expr *exprpb.Expr) (string, bool) {
	call := expr.GetCallExpr()
	if call == nil || call.Target != nil || call.Function != operators.Index || len(call.Args) != 2 {
		return "", false
	}
	if call.Args[0].GetIdentExpr().GetName() != "fields" {
		return "", false
	}
	constant := call.Args[1].GetConstExpr()
	if constant == nil {
		return "", false
	}
	if _, ok := constant.ConstantKind.(*exprpb.Constant_StringValue); !ok {
		return "", false
	}
	name := constant.GetStringValue()
	return name, name != ""
}

func scalarConstant(expr *exprpb.Expr) bool {
	constant := expr.GetConstExpr()
	if constant == nil {
		return false
	}
	switch constant.ConstantKind.(type) {
	case *exprpb.Constant_StringValue, *exprpb.Constant_BoolValue,
		*exprpb.Constant_Int64Value, *exprpb.Constant_Uint64Value,
		*exprpb.Constant_DoubleValue:
		return true
	default:
		return false
	}
}

func unsupportedExpressionError() error {
	return fmt.Errorf(`unsupported CEL expression: compare fields["name"] with string, number, or bool constants using ==, !=, <, <=, >, or >=, combined with the &&, ||, and ! operators`)
}

type direction bool

const descending direction = true

// Order is a deterministic, ordered list of provider-native field keys.
type Order struct {
	terms []orderTerm
}

type orderTerm struct {
	field     string
	direction direction
}

var fieldNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseOrder parses a comma-separated field[:asc|desc] list.
func ParseOrder(expression string) (Order, error) {
	if expression == "" {
		return Order{}, nil
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return Order{}, fmt.Errorf("field order must not be blank")
	}
	var order Order
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(expression, ",") {
		parts := strings.Split(strings.TrimSpace(raw), ":")
		if len(parts) > 2 || parts[0] == "" || !fieldNamePattern.MatchString(parts[0]) {
			return Order{}, fmt.Errorf("invalid field order term %q (want field[:asc|desc])", raw)
		}
		term := orderTerm{field: parts[0]}
		if len(parts) == 2 {
			switch strings.ToLower(parts[1]) {
			case "asc":
			case "desc":
				term.direction = descending
			default:
				return Order{}, fmt.Errorf("invalid field order direction %q for %q (want asc or desc)", parts[1], parts[0])
			}
		}
		if _, ok := seen[term.field]; ok {
			return Order{}, fmt.Errorf("field order repeats %q", term.field)
		}
		seen[term.field] = struct{}{}
		order.terms = append(order.terms, term)
	}
	return order, nil
}

// Validate checks that every item exposes each ordered field with compatible
// scalar types.
func (o Order) Validate(items []Fields) error {
	for _, term := range o.terms {
		expectedKind := ""
		for i, fields := range items {
			value, ok := fields[term.field]
			if !ok || value == nil {
				return fmt.Errorf("field %q is unavailable on item %d", term.field, i)
			}
			kind, err := scalarKind(value)
			if err != nil {
				return fmt.Errorf("field %q on item %d: %w", term.field, i, err)
			}
			if expectedKind == "" {
				expectedKind = kind
			} else if kind != expectedKind {
				return fmt.Errorf("field %q has incompatible %s and %s values", term.field, expectedKind, kind)
			}
		}
	}
	return nil
}

// Compare compares two field sets according to the configured terms.
func (o Order) Compare(left, right Fields) (int, error) {
	for _, term := range o.terms {
		comparison, err := compareScalar(left[term.field], right[term.field])
		if err != nil {
			return 0, fmt.Errorf("compare field %q: %w", term.field, err)
		}
		if comparison == 0 {
			continue
		}
		if term.direction == descending {
			comparison = -comparison
		}
		return comparison, nil
	}
	return 0, nil
}

func scalarKind(value any) (string, error) {
	switch value.(type) {
	case string:
		return "string", nil
	case bool:
		return "bool", nil
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		if _, err := number(value); err != nil {
			return "", err
		}
		return "number", nil
	default:
		return "", fmt.Errorf("unsupported value type %T (want string, number, or bool)", value)
	}
}

func compareScalar(left, right any) (int, error) {
	leftKind, err := scalarKind(left)
	if err != nil {
		return 0, err
	}
	rightKind, err := scalarKind(right)
	if err != nil {
		return 0, err
	}
	if leftKind != rightKind {
		return 0, fmt.Errorf("incompatible %s and %s values", leftKind, rightKind)
	}
	switch leftKind {
	case "string":
		return strings.Compare(left.(string), right.(string)), nil
	case "bool":
		leftBool, rightBool := left.(bool), right.(bool)
		switch {
		case leftBool == rightBool:
			return 0, nil
		case !leftBool:
			return -1, nil
		default:
			return 1, nil
		}
	default:
		leftNumber, _ := number(left)
		rightNumber, _ := number(right)
		switch {
		case leftNumber < rightNumber:
			return -1, nil
		case leftNumber > rightNumber:
			return 1, nil
		default:
			return 0, nil
		}
	}
}

func number(value any) (float64, error) {
	var result float64
	switch typed := value.(type) {
	case int:
		result = float64(typed)
	case int8:
		result = float64(typed)
	case int16:
		result = float64(typed)
	case int32:
		result = float64(typed)
	case int64:
		result = float64(typed)
	case uint:
		result = float64(typed)
	case uint8:
		result = float64(typed)
	case uint16:
		result = float64(typed)
	case uint32:
		result = float64(typed)
	case uint64:
		result = float64(typed)
	case float32:
		result = float64(typed)
	case float64:
		result = typed
	default:
		return 0, fmt.Errorf("unsupported numeric value %s", strconv.Quote(fmt.Sprint(value)))
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, fmt.Errorf("non-finite numeric value")
	}
	return result, nil
}
