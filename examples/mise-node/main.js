// Counts the words of a text file with the job's own pinned Node.js.
// Usage: node main.js IN OUT
const fs = require("fs");

const [input, output] = process.argv.slice(2);
const text = fs.readFileSync(input, "utf8");
const words = text.split(/\s+/).filter(Boolean);
console.log("UMM_PROGRESS=50");
const counts = {};
for (const w of words) counts[w.toLowerCase()] = (counts[w.toLowerCase()] || 0) + 1;
const top = Object.entries(counts).sort((a, b) => b[1] - a[1]).slice(0, 10);
fs.writeFileSync(output, JSON.stringify({ node: process.version, words: words.length, top }, null, 2) + "\n");
console.log(`node ${process.version}: ${words.length} words`);
