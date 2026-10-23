package schema

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// function is one routine in a non-system schema that no extension owns.
type function struct {
	schema, name, arguments, owner string
	securityDefiner                bool
}

func (f function) signature() string { return f.schema + "." + f.name + "(" + f.arguments + ")" }

func (f function) target() string {
	return ident(f.schema) + "." + ident(f.name) + "(" + f.arguments + ")"
}

const listFunctions = `
SELECT n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), pg_get_userbyid(p.proowner), p.prosecdef
FROM pg_proc AS p JOIN pg_namespace AS n ON n.oid = p.pronamespace
WHERE ` + systemSchemas + `
  AND NOT EXISTS (SELECT 1 FROM pg_depend AS d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
ORDER BY 1, 2, 3`

func loadFunctions(ctx context.Context, tx pgx.Tx) ([]function, error) {
	rows, err := tx.Query(ctx, listFunctions)
	if err != nil {
		return nil, fmt.Errorf("list cloud functions: %w", err)
	}
	functions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (function, error) {
		var f function
		return f, row.Scan(&f.schema, &f.name, &f.arguments, &f.owner, &f.securityDefiner)
	})
	if err != nil {
		return nil, fmt.Errorf("list cloud functions: %w", err)
	}
	return functions, nil
}

// functionStatements revokes EXECUTE on every function from PUBLIC and the
// runtime roles, then grants it as the inventory lists. Functions default to
// PUBLIC EXECUTE, and a SECURITY DEFINER one runs with its owner's rights, so
// it is refused unless the inventory declares it as such.
func functionStatements(inventory Inventory, functions []function) ([]string, error) {
	policies := make(map[string]Function, len(inventory.Functions))
	for _, policy := range inventory.Functions {
		policies[policy.Signature()] = policy
	}
	roles := strings.Join(RuntimeRoles, ", ")
	var statements []string
	for _, f := range functions {
		if slices.Contains(RuntimeRoles, f.owner) {
			return nil, fmt.Errorf("%w: %s owns function %s", ErrRolePrivileged, f.owner, f.signature())
		}
		policy, listed := policies[f.signature()]
		if f.securityDefiner && (!listed || !policy.SecurityDefiner) {
			return nil, fmt.Errorf("cloud function %s is SECURITY DEFINER without a write-path inventory entry declaring it", f.signature())
		}
		statements = append(statements, fmt.Sprintf("REVOKE ALL ON FUNCTION %s FROM PUBLIC, %s", f.target(), roles))
		if listed && len(policy.Execute) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT EXECUTE ON FUNCTION %s TO %s", f.target(), strings.Join(policy.Execute, ", ")))
		}
	}
	return statements, nil
}

// effectiveFunctionPrivileges reports which runtime roles can execute which
// non-extension functions; Column carries the identity arguments.
const effectiveFunctionPrivileges = `
SELECT r.role, n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), 'EXECUTE'
FROM unnest($1::text[]) AS r(role)
CROSS JOIN pg_proc AS p JOIN pg_namespace AS n ON n.oid = p.pronamespace
WHERE ` + systemSchemas + `
  AND NOT EXISTS (SELECT 1 FROM pg_depend AS d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`
