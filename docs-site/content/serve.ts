import {resolve,sep} from 'node:path';
const root=resolve(import.meta.dir,process.env.DOCS_PREVIEW==='storybook'?'../storybook-static':'../build/client');
const port=Number(process.env.DOCS_PORT??0);
if(!Number.isInteger(port)||port<0)throw new Error('Invalid DOCS_PORT');
const server=Bun.serve({hostname:'127.0.0.1',port,async fetch(request){let path;try{path=decodeURIComponent(new URL(request.url).pathname);}catch{return new Response('Bad path',{status:400});}let file=resolve(root,'.'+path);if(file!==root&&!file.startsWith(root+sep))return new Response('Forbidden',{status:403});if(!/\.[^/]+$/.test(file))file=resolve(file,'index.html');const blob=Bun.file(file);return await blob.exists()?new Response(blob):new Response('Not found',{status:404});}});
console.log(`Documentation preview: ${server.url}`);
