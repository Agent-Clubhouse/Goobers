export function findingsFromKnipReport(report) {
  const findings = (report.files ?? []).map((file) => ({
    type: "files",
    file,
    symbol: file,
  }));

  for (const issue of report.issues ?? []) {
    for (const type of ["files", "exports", "nsExports"]) {
      for (const finding of issue[type] ?? []) {
        findings.push({ type, file: issue.file, symbol: finding.name });
      }
    }
  }

  return findings;
}

export function reviewFindings(findings, exemptions) {
  const keyOf = ({ type, file, symbol }) => `${type}:${file}:${symbol}`;
  const exemptionByKey = new Map();
  for (const exemption of exemptions) {
    const key = keyOf(exemption);
    if (exemptionByKey.has(key)) {
      throw new Error(`duplicate dead-code exemption: ${key}`);
    }
    if (!exemption.reason) {
      throw new Error(`dead-code exemption has no reason: ${key}`);
    }
    exemptionByKey.set(key, exemption);
  }

  const findingKeys = new Set(findings.map(keyOf));
  return {
    unexpected: findings.filter((finding) => !exemptionByKey.has(keyOf(finding))),
    stale: exemptions.filter((exemption) => !findingKeys.has(keyOf(exemption))),
  };
}
