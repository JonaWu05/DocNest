import { build } from "esbuild";

await build({
  entryPoints: ["web/js/main.js"],
  bundle: true,
  platform: "browser",
  write: false,
  logLevel: "info",
});
