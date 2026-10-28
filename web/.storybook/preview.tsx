import type { Preview } from "@storybook/react";
import "../src/app/styles.css";

const preview: Preview = {
  parameters: {
    controls: { expanded: true },
    layout: "fullscreen",
    backgrounds: {
      default: "frame",
      values: [{ name: "frame", value: "#f9fbfc" }],
    },
  },
};

export default preview;
