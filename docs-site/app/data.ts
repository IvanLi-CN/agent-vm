import generated from '../.generated/source.json';
type Identity = Omit<typeof generated.identity, 'modifiedPaths'> & { modifiedPaths: string[] };
export default generated as Omit<typeof generated,'snapshots'|'contentDiagnostics'|'identity'> & { identity: Identity; snapshots:Record<string,string>;contentDiagnostics:{path:string;message:string}[] };
