// EmbeddingGemma embed server for the Stage 1b benchmark (qmd's runner: the GGUF
// run in-process via node-llama-cpp's prebuilt llama.cpp — no Ollama, no build).
// Exposes Ollama's /api/embed shape so the brain's opt-in embedder can reach it:
//   POST {"input": "..."} -> {"embeddings": [[...768 floats...]]}
//
// Setup (see README.md):
//   npm install node-llama-cpp
//   # canonical GGUF (open, not Gemma-gated):
//   #   ggml-org/embeddinggemma-300M-GGUF / embeddinggemma-300M-Q8_0.gguf
//   GGUF=/path/to/embeddinggemma-300M-Q8_0.gguf PORT=11500 node embed-server.mjs
//
// Then point the brain at it:
//   ENTIRE_BRAIN_EMBEDDER=ollama ENTIRE_BRAIN_EMBED_URL=http://localhost:11500 \
//     entire-brain facts eval --tasks tasks.json --branch main --semantic --json
import http from "node:http";
import { getLlama } from "node-llama-cpp";

const modelPath = process.env.GGUF;
const port = Number(process.env.PORT || 11500);
if (!modelPath) {
  console.error("set GGUF=/path/to/embeddinggemma-300M-Q8_0.gguf");
  process.exit(1);
}

console.log("loading", modelPath);
const llama = await getLlama();
const model = await llama.loadModel({ modelPath });
const ctx = await model.createEmbeddingContext();
const probe = await ctx.getEmbeddingFor("title: none | text: probe");
console.log("ready: dim =", probe.vector.length, "on :" + port);

http.createServer((req, res) => {
  if (req.method !== "POST") { res.writeHead(405); res.end(); return; }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", async () => {
    try {
      const { input } = JSON.parse(body);
      const emb = await ctx.getEmbeddingFor(String(input));
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ embeddings: [Array.from(emb.vector)] }));
    } catch (e) {
      res.writeHead(500, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: String(e) }));
    }
  });
}).listen(port, () => console.log("embed server listening on :" + port));
