/** Wraps a Metro config so every bundle it makes carries a new debug id (see metro.cjs). */
export function withJujuConfig<T>(config: T): T;
/** The snippet added to each bundle, for an id. */
export function snippet(id: string): string;
