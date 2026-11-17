import {defineConfig,devices} from '@playwright/test';
const port=process.env.DOCS_PORT;
if(!port)throw new Error('Set DOCS_PORT from task port leases (CI uses isolated ports)');
export default defineConfig({testDir:'./tests',testMatch:'*.spec.ts',use:{baseURL:`http://127.0.0.1:${port}`},projects:[{name:'desktop',use:{...devices['Desktop Chrome'],viewport:{width:1280,height:900}}},{name:'tablet',use:{viewport:{width:1024,height:900}}},{name:'mobile',use:{viewport:{width:393,height:852}}}],webServer:[{command:'bun content/serve.ts',url:`http://127.0.0.1:${port}`,reuseExistingServer:false}]});
