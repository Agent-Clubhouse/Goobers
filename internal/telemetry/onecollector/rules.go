package onecollector

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"
)

func celNative(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if !strings.ContainsAny(typed.String(), ".eE") {
			if signed, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
				return signed
			}
			if unsigned, err := strconv.ParseUint(typed.String(), 10, 64); err == nil {
				return unsigned
			}
			return typed.String()
		}
		if number, err := typed.Float64(); err == nil {
			return number
		}
		return typed.String()
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = celNative(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = celNative(child)
		}
		return result
	default:
		return value
	}
}

func fromCEL(value ref.Val) any {
	switch typed := value.(type) {
	case traits.Mapper:
		result := map[string]any{}
		iterator := typed.Iterator()
		for iterator.HasNext() == types.True {
			key := iterator.Next()
			result[fmt.Sprint(key.Value())] = fromCEL(typed.Get(key))
		}
		return result
	case traits.Lister:
		result := []any{}
		iterator := typed.Iterator()
		for iterator.HasNext() == types.True {
			result = append(result, fromCEL(iterator.Next()))
		}
		return result
	default:
		return value.Value()
	}
}

func valueDepth(value any) int {
	depth := 0
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			depth = max(depth, valueDepth(child))
		}
		return depth + 1
	case []any:
		for _, child := range typed {
			depth = max(depth, valueDepth(child))
		}
		return depth + 1
	default:
		return 0
	}
}

func uniqueStringList(value ref.Val) ref.Val {
	list, ok := value.(traits.Lister)
	if !ok {
		return types.NewErr("invalid uniqueness input")
	}
	size, ok := list.Size().(types.Int)
	if !ok || size < 0 || size > MaxNativeBytes {
		return types.NewErr("invalid uniqueness input")
	}
	seen := map[string]bool{}
	total := 0
	iterator := list.Iterator()
	for iterator.HasNext() == types.True {
		text, ok := iterator.Next().Value().(string)
		if !ok {
			return types.NewErr("uniqueness requires strings")
		}
		total += len(text)
		if total > MaxNativeBytes {
			return types.NewErr("uniqueness byte limit")
		}
		if seen[text] {
			return types.False
		}
		seen[text] = true
	}
	return types.True
}

func ruleEnvironment() (*cel.Env, error) {
	stringHelper := func(name string, output *cel.Type, call func(string) ref.Val) cel.EnvOption {
		return cel.Function(name, cel.Overload(name+"_string", []*cel.Type{cel.StringType}, output, cel.UnaryBinding(func(value ref.Val) ref.Val {
			text, ok := value.Value().(string)
			if !ok {
				return types.NewErr("invalid helper argument")
			}
			return call(text)
		})))
	}
	return cel.NewEnv(
		ext.Strings(),
		cel.Variable("input", cel.DynType), cel.Variable("tokens", cel.MapType(cel.StringType, cel.StringType)),
		cel.Function("uniqueStrings", cel.Overload("uniqueStrings_list_string", []*cel.Type{cel.ListType(cel.StringType)}, cel.BoolType, cel.UnaryBinding(uniqueStringList))),
		stringHelper("jsonParse", cel.DynType, func(text string) ref.Val {
			value, err := DecodeObject(text, MaxNativeBytes)
			if err != nil {
				return types.NewErr("invalid embedded JSON")
			}
			return types.DefaultTypeAdapter.NativeToValue(celNative(value))
		}),
		cel.Function("jsonParse", cel.Overload("jsonParse_string_bool", []*cel.Type{cel.StringType, cel.BoolType}, cel.DynType, cel.BinaryBinding(func(text, check ref.Val) ref.Val {
			body, ok := text.Value().(string)
			if !ok {
				return types.NewErr("invalid embedded JSON")
			}
			value, err := DecodeObject(body, MaxNativeBytes)
			if err != nil {
				return types.NewErr("invalid embedded JSON")
			}
			if check == types.True {
				if err := normalizeValue(value, false); err != nil {
					return types.NewErr("embedded normalized collision")
				}
			}
			return types.DefaultTypeAdapter.NativeToValue(celNative(value))
		}))),
		stringHelper("utf16Length", cel.IntType, func(text string) ref.Val { return types.Int(len(utf16.Encode([]rune(text)))) }),
		stringHelper("canonicalBase64", cel.BoolType, func(text string) ref.Val {
			raw, err := base64.StdEncoding.DecodeString(text)
			return types.Bool(err == nil && base64.StdEncoding.EncodeToString(raw) == text)
		}),
		stringHelper("signedDecimal", cel.BoolType, func(text string) ref.Val {
			_, err := strconv.ParseInt(text, 10, 64)
			return types.Bool(err == nil && regexp.MustCompile(`^(0|-[1-9][0-9]*|[1-9][0-9]*)$`).MatchString(text))
		}),
		stringHelper("unsignedDecimal", cel.BoolType, func(text string) ref.Val {
			_, err := strconv.ParseUint(text, 10, 64)
			return types.Bool(err == nil && regexp.MustCompile(`^(0|[1-9][0-9]*)$`).MatchString(text))
		}),
		stringHelper("validTimestamp", cel.BoolType, func(text string) ref.Val {
			value, err := time.Parse(time.RFC3339Nano, text)
			valid := err == nil && value.Year() >= 1 && value.Year() <= 9999
			if valid {
				_, offset := value.Zone()
				valid = offset > -(24*60*60) && offset < 24*60*60
			}
			return types.Bool(valid)
		}),
		cel.Function("jsonToken", cel.Overload("jsonToken_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.StringType, cel.BinaryBinding(func(text, path ref.Val) ref.Val {
			body, ok := text.Value().(string)
			if !ok {
				return types.NewErr("invalid JSON token input")
			}
			key, ok := path.Value().(string)
			if !ok {
				return types.NewErr("invalid JSON token path")
			}
			object, err := DecodeObject(body, MaxNativeBytes)
			if err != nil {
				return types.NewErr("invalid JSON")
			}
			value, found := selectValue(object, key)
			if !found {
				return types.NewErr("missing JSON token")
			}
			return types.String(string(JSON(value)))
		}))),
		cel.Function("jsonStringify", cel.Overload("jsonStringify_dyn", []*cel.Type{cel.DynType}, cel.StringType, cel.UnaryBinding(func(value ref.Val) ref.Val {
			raw, err := compactJSON(fromCEL(value))
			if err != nil {
				return types.NewErr("cannot encode JSON")
			}
			return types.String(raw)
		}))),
		cel.Function("jsonDepth", cel.Overload("jsonDepth_dyn", []*cel.Type{cel.DynType}, cel.IntType, cel.UnaryBinding(func(value ref.Val) ref.Val { return types.Int(valueDepth(fromCEL(value))) }))),
		cel.Function("jsonObjects", cel.Overload("jsonObjects_dyn", []*cel.Type{cel.DynType}, cel.ListType(cel.DynType), cel.UnaryBinding(func(value ref.Val) ref.Val {
			objects := []any{}
			var visit func(any)
			visit = func(value any) {
				switch typed := value.(type) {
				case map[string]any:
					objects = append(objects, typed)
					for _, child := range typed {
						visit(child)
					}
				case []any:
					for _, child := range typed {
						visit(child)
					}
				}
			}
			visit(fromCEL(value))
			return types.DefaultTypeAdapter.NativeToValue(objects)
		}))),
	)
}

func compileRule(expression string) (cel.Program, error) {
	if len(expression) > 16<<10 {
		return nil, errors.New("onecollector: rule byte limit")
	}
	env, err := ruleEnvironment()
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil || !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, errors.New("onecollector: invalid Boolean validation rule")
	}
	program, err := env.Program(ast, cel.CostLimit(100000), cel.InterruptCheckFrequency(100))
	if err != nil {
		return nil, errors.New("onecollector: invalid validation program")
	}
	return program, nil
}

func evaluateRules(rules []string, object any) error {
	facts := inputFacts(object, "")
	tokens := make(map[string]string, len(facts))
	for key, value := range facts {
		tokens[strings.TrimPrefix(key, "input.")] = string(value)
	}
	for _, expression := range rules {
		program, err := compileRule(expression)
		if err != nil {
			return err
		}
		value, _, err := program.Eval(map[string]any{"input": celNative(object), "tokens": tokens})
		if err != nil || value != types.True {
			return errors.New("onecollector: semantic validation failed")
		}
	}
	return nil
}
