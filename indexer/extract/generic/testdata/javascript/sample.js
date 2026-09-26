import fs from "fs";
import { join as pjoin, resolve } from "path";
import * as util from "./util.js";
export { helper as help } from "./helpers.js";
export * from "./all.js";
const lodash = require("lodash");

/**
 * Widget renders things.
 */
export class Widget extends Base {
  static count = 0;
  #secret = 1;

  constructor(opts) {
    super(opts);
    this.opts = opts;
  }

  async render() {
    util.log("render");
    pjoin("a", "b");
    this.draw();
    return new Canvas(10);
  }
}

// Adds numbers.
export function add(a, b) {
  return a + b;
}

const multiply = (a, b) => a * b;

function internal() {
  add(1, 2);
}

export default Widget;
