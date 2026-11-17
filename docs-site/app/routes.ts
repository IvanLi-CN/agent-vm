import {index,route,type RouteConfig} from '@react-router/dev/routes';
export default [index('routes/home.tsx'),route('review/examples','routes/examples.tsx'),route('review','routes/review.tsx'),route('source','routes/source.tsx'),route('docs/:slug','routes/document.tsx')] satisfies RouteConfig;
