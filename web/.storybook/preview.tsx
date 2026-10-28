import type { Preview } from "@storybook/react";
import "../src/app/styles.css";

const preview: Preview = {
  parameters: {
    a11y: { test: "todo" },
    controls: { expanded: true },
    layout: "fullscreen",
    backgrounds: {
      default: "frame",
      values: [{ name: "frame", value: "#f9fbfc" }],
    },
  },
};

export default preview;
