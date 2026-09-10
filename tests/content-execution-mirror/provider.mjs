import http from "node:http";

// Synthetic identities and responses. Bound only to the isolated fixture
// network; never forward prompts, credentials, or notifications externally.
http.createServer(async (request, response) => {
  if (request.method !== "POST") { response.writeHead(404).end(); return; }
  let raw = "";
  for await (const chunk of request) {
    raw += chunk;
    if (raw.length > 2_000_000) { response.writeHead(413).end(); return; }
  }
  let body;
  try { body = JSON.parse(raw); } catch { response.writeHead(400).end(); return; }
  if (request.url === "/v1/auth/verify") {
    const uid = { "fixture-owner": "owner", "fixture-outsider": "outsider" }[body.token];
    if (!uid) { response.writeHead(401).end(); return; }
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ uid, name: uid }));
    return;
  }
  if (request.url !== "/v1/chat/completions") { response.writeHead(404).end(); return; }
  if (body.tools?.length) { response.writeHead(400).end("retrieval tools are not allowed"); return; }
  const prompt = JSON.stringify(body.messages);
  if (prompt.includes("fixture:fail")) { response.writeHead(400).end("synthetic model failure"); return; }
  if (prompt.includes("fixture:slow")) await new Promise((resolve) => setTimeout(resolve, 2500));
  const content = "Alpha release shipped successfully [1]";
  if (body.stream) {
    response.writeHead(200, { "content-type": "text/event-stream" });
    response.write(`data: ${JSON.stringify({ choices: [{ delta: { content }, finish_reason: null }] })}\n\n`);
    response.end('data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":10}}\n\ndata: [DONE]\n\n');
  } else {
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ choices: [{ message: { role: "assistant", content }, finish_reason: "stop" }], usage: { total_tokens: 10 } }));
  }
}).listen(8089, "0.0.0.0");
