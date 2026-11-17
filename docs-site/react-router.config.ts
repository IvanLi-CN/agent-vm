import type { Config } from '@react-router/dev/config';
import { catalog } from './content/catalog';
export default { ssr: false, prerender: ['/', '/source', ...catalog.map(doc => '/docs/' + doc.slug)] } satisfies Config;
