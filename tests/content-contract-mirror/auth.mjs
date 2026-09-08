import http from "node:http";

// Synthetic identities for the isolated test network only. This fixture must
// never be used as a production authentication service.
http.createServer(async (request, response) => {
  if (request.url !== "/v1/auth/verify" || request.method !== "POST") {
    response.writeHead(404).end();
    return;
  }
  let body = "";
  for await (const chunk of request) {
    body += chunk;
    if (body.length > 4096) {
      response.writeHead(413).end();
      return;
    }
  }
  let token;
  try { token = JSON.parse(body).token; } catch {
    response.writeHead(400).end();
    return;
  }
  const users = new Map([
    ["fixture-owner", "owner"], ["fixture-member", "member"],
    ["fixture-outsider", "outsider"],
  ]);
  const uid = users.get(token);
  if (!uid) {
    response.writeHead(401).end();
    return;
  }
  response.writeHead(200, { "content-type": "application/json" });
  response.end(JSON.stringify({ uid, name: uid }));
}).listen(8089, "0.0.0.0");
