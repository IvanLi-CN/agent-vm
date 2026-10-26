// Source-tree adapter: the release bundle places the real support module next
// to the bridge, while repository checks resolve this import from the public
// Agent VM client implementation.
export { runBounded } from "../../agent-vm/scripts/client-io.ts";
