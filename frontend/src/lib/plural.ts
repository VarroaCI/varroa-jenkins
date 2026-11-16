/** plural answers the count and the noun that agrees with it, for example "1 run" or "2 runs". */
export function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}
