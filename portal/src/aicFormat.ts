const wholeAICFormat = new Intl.NumberFormat("en-US", {
  maximumFractionDigits: 0,
  minimumFractionDigits: 0,
});

export function formatAIC(value: number): string {
  return `${wholeAICFormat.format(value)} AIC`;
}
