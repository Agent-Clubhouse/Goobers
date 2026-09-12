// Reject direct property interpolation at an innerHTML sink. Builders may
// concatenate escaped values; this catches the regression shape behind #4567
// and #4892 while allowing constant markup and pre-scrubbed builder results.
export function lintInnerHTMLAssignments(source) {
    const findings = [];
    for (const match of source.matchAll(/\.innerHTML\s*=\s*([\s\S]*?);/g)) {
        const rhs = match[1];
        if (!rhs.includes("+")) continue;
        const withoutEscapedValues = rhs.replace(/escapeHtml\((?:[^()]|\([^()]*\))*\)/g, "");
        let rawIdentifier;
        for (const candidate of withoutEscapedValues.matchAll(/(?:^|\+)\s*\(?\s*([A-Za-z_$][\w$]*(?:(?:\?\.)?\.[A-Za-z_$][\w$]*)?)/g)) {
            const remainder = withoutEscapedValues.slice(candidate.index + candidate[0].length).trimStart();
            const name = candidate[1];
            if (remainder.startsWith("(") || remainder.startsWith("?") || /(?:Html|Markup|Attr)$/.test(name)) continue;
            rawIdentifier = candidate;
            break;
        }
        if (!rawIdentifier) continue;
        const line = source.slice(0, match.index).split("\n").length;
        findings.push(`line ${line}: innerHTML concatenates unescaped ${rawIdentifier[1]}`);
    }
    return findings;
}
