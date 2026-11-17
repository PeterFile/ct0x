// Controlled test descendant, NEVER the Claude CLI or SDK.
const fs = require("node:fs");
process.stdout.write("controlled-native-stdout-tail");
process.stderr.write("controlled-native-stderr-tail");
setTimeout(() => {
  if (process.argv[2]) fs.writeFileSync(process.argv[2], "controlled descendant completed", { mode: 0o600 });
  process.exit(0);
}, process.argv[2] ? 300 : 80);
