import type { StorybookConfig } from '@storybook/react-vite';
import tailwind from '@tailwindcss/vite';
const config: StorybookConfig = {
  core:{disableTelemetry:true},
  stories: ['../app/components/*.stories.tsx'],
  addons: ['@storybook/addon-essentials', '@storybook/addon-a11y', '@storybook/addon-interactions'],
  framework: {name:'@storybook/react-vite',options:{builder:{viteConfigPath:'.storybook/vite.config.ts'}}},
  viteFinal: async config => ({...config,plugins:[...(config.plugins??[]),tailwind()]})
};
export default config;
