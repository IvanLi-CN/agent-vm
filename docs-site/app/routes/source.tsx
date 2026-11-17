import {useEffect} from 'react';
import {useSearchParams} from 'react-router';
import data from '../data';
export default function Source(){const [params]=useSearchParams();const path=params.get('path')??data.documents[0].path;useEffect(()=>{const line=params.get('line');if(line&&/^\d+$/.test(line))document.getElementById('L'+line)?.scrollIntoView({block:'center'});},[params]);const text=data.snapshots[path];return <><h1>构建来源</h1><p>{path}</p><p>此快照对应当前预览内容，不受之后的工作区编辑影响。</p>{text?<pre>{text.split('\n').map((line,i)=><div id={'L'+(i+1)} key={i} style={{background:params.get('line')===String(i+1)?'var(--color-fd-accent)':undefined}}><span className="line">{i+1}</span>{line||' '}</div>)}</pre>:<p role="alert">来源未纳入本次构建。</p>}</>}
