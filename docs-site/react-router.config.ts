import type {Config} from '@react-router/dev/config';
export default {ssr:false,prerender:['/','/review','/review/examples','/source',...Array.from({length:7},(_,i)=>'/docs/'+i)]} satisfies Config;
