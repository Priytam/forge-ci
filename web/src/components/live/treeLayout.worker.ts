/// <reference lib="webworker" />
// Lays big resource trees out off the main thread.
import { layoutTree, type LayoutInput } from "./treeLayout";

self.onmessage = (e: MessageEvent<{ seq: number; input: LayoutInput }>) => {
  self.postMessage({ seq: e.data.seq, layout: layoutTree(e.data.input) });
};
