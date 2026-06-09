// EmbeddingGemma embed server for the Stage 1b benchmark (qmd's runner: the GGUF
// runs in-process via node-llama-cpp's prebuilt llama.cpp — no Ollama, no build).
// Exposes Ollama's /api/embed shape so the brain's opt-in embedder can reach it.
// The brain's ollamaEmbedder POSTs {"model": "…", "input": "…"}; only "input" is
// used here (extra fields like "model" are ignored):
//   POST {"model": "…", "input": "…"} -> {"embeddings": [[...768 floats...]]}
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
// Bind loopback by default so this compute-heavy endpoint isn't exposed on a
// shared machine; set HOST=0.0.0.0 to opt into all interfaces.
const host = process.env.HOST || "127.0.0.1";
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
  const chunks = [];
  let size = 0;
  let aborted = false;
  const MAX_BODY = 1 << 20; // 1 MiB — embed inputs are short; cap accidental/huge requests
  req.on("data", (c) => {
    if (aborted) return;
    size += c.length; // c is a Buffer; .length is bytes, not UTF-16 code units
    if (size > MAX_BODY) {
      aborted = true;
      res.writeHead(413, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "request body too large" }));
      req.destroy();
      return;
    }
    chunks.push(c);
  });
  req.on("end", async () => {
    if (aborted) return;
    // 400 for a malformed/incomplete request; 500 only for embedder/runtime faults.
    let input;
    try {
      ({ input } = JSON.parse(Buffer.concat(chunks).toString("utf8")));
    } catch {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "invalid JSON body" }));
      return;
    }
    if (typeof input !== "string" || input.length === 0) {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "missing or empty 'input' string" }));
      return;
    }
    try {
      const emb = await ctx.getEmbeddingFor(input);
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ embeddings: [Array.from(emb.vector)] }));
    } catch (e) {
      res.writeHead(500, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: String(e) }));
    }
  });
}).listen(port, host, () => console.log(`embed server listening on ${host}:${port}`));
