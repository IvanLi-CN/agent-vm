import type {Config} from '@react-router/dev/config';
export default {ssr:false,prerender:['/','/source',...Array.from({length:7},(_,i)=>'/docs/'+i)]} satisfies Config;
