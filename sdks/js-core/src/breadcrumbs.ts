import type { Breadcrumb } from "./types";

/**
 * A ring buffer: it keeps the most recent `max` breadcrumbs and drops the
 * oldest when full, so memory stays flat however long the app runs. The
 * default of 100 matches what the server keeps.
 */
export class Breadcrumbs {
  private items: Breadcrumb[] = [];

  constructor(private readonly max = 100) {}

  add(crumb: Breadcrumb): void {
    this.items.push(crumb);
    if (this.items.length > this.max) this.items.shift();
  }

  /** A copy, so later breadcrumbs don't change an event already built. */
  snapshot(): Breadcrumb[] {
    return this.items.slice();
  }

  clear(): void {
    this.items = [];
  }
}
