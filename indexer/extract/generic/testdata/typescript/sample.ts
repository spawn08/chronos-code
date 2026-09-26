import { Injectable } from "@angular/core";
import type { Repo } from "./repo";
import * as path from "path";
export { Service as Svc } from "./service";

/** Storage abstraction. */
export interface Store<T> extends Reader {
  get(id: string): T;
}

export type Id = string | number;

export enum Color {
  Red,
  Green = "green",
}

@Injectable()
export class UserService extends BaseService implements Store<User>, Disposable {
  private readonly cache = new Map<string, User>();
  protected static instances: number = 0;

  constructor(private repo: Repo) {
    super();
  }

  public get(id: string): User {
    return this.repo.find(id);
  }

  async dispose(): Promise<void> {
    path.join("a");
    await flush();
  }
}

export abstract class Shape {
  abstract area(): number;
}

namespace Geometry {
  export function area(r: number): number {
    return Math.PI * r * r;
  }
}

declare module "legacy" {
  export function old(): void;
}

export const DEFAULT_ID: Id = 0;
let counter = 0;

function helper(x: number): number {
  return x + counter;
}
