import generated from '../.generated/source.json';
export default generated as Omit<typeof generated,'snapshots'|'contentDiagnostics'> & {snapshots:Record<string,string>;contentDiagnostics:{path:string;message:string}[]};
