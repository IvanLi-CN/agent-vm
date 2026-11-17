import generated from '../.generated/review.json';
import type { trace } from '../content/trace';
export default generated as Omit<typeof generated, 'requirements' | 'diagnostics' | 'snapshots' | 'contentDiagnostics'> & { snapshots: Record<string,string>; contentDiagnostics: {path:string;message:string}[] } & ReturnType<typeof trace>;
