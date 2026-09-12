// A deliberately small source guard for the extension's browser bundle. It
// follows values into a local HTML/markup builder so moving an unsafe
// concatenation one statement away from an innerHTML sink does not evade the
// check. This is not a JavaScript parser; it pins the interpolation shapes the
// extension permits at this trust boundary.

function maskStrings(source) {
    let masked = "";
    const templates = [];
    for (let index = 0; index < source.length;) {
        const quote = source[index];
        if (quote !== '"' && quote !== "'" && quote !== "`") {
            masked += quote;
            index += 1;
            continue;
        }
        const template = quote === "`";
        masked += " ";
        index += 1;
        while (index < source.length) {
            if (source[index] === "\\") {
                masked += "  ";
                index += 2;
                continue;
            }
            if (template && source[index] === "$" && source[index + 1] === "{") {
                const start = index + 2;
                let depth = 1;
                index = start;
                while (index < source.length && depth > 0) {
                    if (source[index] === "{") depth += 1;
                    if (source[index] === "}") depth -= 1;
                    index += 1;
                }
                const expression = source.slice(start, index - 1);
                templates.push(expression);
                masked += " ${" + expression + "}";
                continue;
            }
            masked += " ";
            if (source[index] === quote) {
                index += 1;
                break;
            }
            index += 1;
        }
    }
    return { masked, templates };
}

function stripSafeCalls(expression) {
    let output = expression;
    for (;;) {
        const match = /\bescapeHtml\s*\(/.exec(output);
        if (!match) return output;
        let depth = 1;
        let index = match.index + match[0].length;
        let quote = "";
        for (; index < output.length && depth > 0; index += 1) {
            const character = output[index];
            if (quote) {
                if (character === "\\") index += 1;
                else if (character === quote) quote = "";
                continue;
            }
            if (character === '"' || character === "'" || character === "`") quote = character;
            else if (character === "(") depth += 1;
            else if (character === ")") depth -= 1;
        }
        output = output.slice(0, match.index) + " SAFE " + output.slice(index);
    }
}

function unsafeInterpolation(expression) {
    const safeStripped = stripSafeCalls(expression);
    const { masked, templates } = maskStrings(safeStripped);
    for (const template of templates) {
        const unsafe = unsafeInterpolation("+ " + template);
        if (unsafe) return unsafe;
    }
    const stringWrapped = /\bString\s*\(\s*([A-Za-z_$][\w$]*(?:(?:\?\.)?\.[A-Za-z_$][\w$]*)+)/.exec(masked);
    if (stringWrapped) return stringWrapped[1];
    if (!masked.includes("+")) return "";
    // Only value positions after a concatenation or ternary delimiter flow to
    // the output. A condition controlling two literal alternatives does not.
    for (const candidate of masked.matchAll(/(?:^|[+?:])\s*\(*\s*([A-Za-z_$][\w$]*(?:(?:\?\.)?\.[A-Za-z_$][\w$]*)*)/g)) {
        const name = candidate[1];
        const remainder = masked.slice(candidate.index + candidate[0].length).trimStart();
        if (name === "SAFE" || /(?:Html|Markup|Attr)$/.test(name)) continue;
        if (remainder.startsWith("?")) continue;
        if (remainder.startsWith("(") && !/^String\b/.test(name)) continue;
        return name;
    }
    return "";
}

function builderExpressions(source, sinkIndex, name) {
    const prefix = source.slice(0, sinkIndex);
    const assignment = new RegExp("(?:\\b(?:let|const|var)\\s+)?\\b" + name + "\\s*(\\+?=)\\s*([\\s\\S]*?);", "g");
    const expressions = [];
    for (const match of prefix.matchAll(assignment)) {
        if (match[1] === "=") expressions.length = 0;
        expressions.push({ operator: match[1], expression: match[2] });
    }
    return expressions;
}

export function lintInnerHTMLAssignments(source) {
    const findings = [];
    for (const match of source.matchAll(/\.innerHTML\s*=\s*([\s\S]*?);/g)) {
        const rhs = match[1];
        let unsafe = unsafeInterpolation(rhs);
        if (!unsafe) {
            const bareBuilder = /^\s*([A-Za-z_$][\w$]*(?:Html|Markup))\s*$/.exec(rhs);
            if (bareBuilder) {
                for (const assignment of builderExpressions(source, match.index, bareBuilder[1])) {
                    unsafe = unsafeInterpolation((assignment.operator === "+=" ? "+ " : "") + assignment.expression);
                    if (unsafe) break;
                }
            }
        }
        if (!unsafe) continue;
        const line = source.slice(0, match.index).split("\n").length;
        findings.push(`line ${line}: innerHTML concatenates unescaped ${unsafe}`);
    }
    return findings;
}
