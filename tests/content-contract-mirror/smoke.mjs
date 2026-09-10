import assert from "node:assert/strict";

const base = process.env.CONTENT_MIRROR_API ?? "http://127.0.0.1:28341";
const options = { headers: { token: "fixture-owner", "X-Space-Id": "fixture-space" } };
async function request(path, requestOptions = options, expectedStatus = 200) {
  const response = await fetch(`${base}/api/v1${path}`, requestOptions);
  assert.equal(response.status, expectedStatus, `${path} status`);
  const envelope = await response.json();
  if (expectedStatus === 200) assert.equal(envelope.code, 0);
  return envelope.data;
}

const group = await request("/summaries/101/contents");
assert.equal(group.contents[0].kind, "result");
assert.equal(group.contents[0].content_revision, 7);
assert.equal(group.contents[0].capabilities.can_edit, false);
const contentID = group.main_content_id;
let cursor = "";
const versions = [];
do {
  const page = await request(`/summaries/101/contents/${contentID}/versions?limit=3&cursor=${encodeURIComponent(cursor)}`);
  versions.push(...page.items);
  cursor = page.next_cursor;
} while (cursor);
assert.deepEqual(versions.map((version) => version.version), [7, 6, 5, 4, 3, 2, 1]);

const personal = await request("/summaries/102/contents");
assert.equal(personal.contents[0].kind, "personal");
assert.equal(personal.contents[0].current_version.provisional, true);
const repeat = await request("/summaries/102/contents");
assert.equal(repeat.contents[0].current_version.version_id, personal.contents[0].current_version.version_id);

const team = await request("/summaries/103/contents");
assert.equal(team.contents.length, 1); // creator is not the reporting member
assert.equal(team.contents[0].kind, "result");
assert.deepEqual(team.contents[0].current_version.citations, []);
assert.equal(team.contents[0].current_version.citation_visibility, "permission_hidden");
const history = await request(`/summaries/103/contents/${team.main_content_id}/versions`);
const historical = history.items.find((version) => !version.is_current);
assert.equal(historical.team_citation_visibility, "historical_identity_only");
assert.equal(historical.team_citations[0].user_name, "Historical Member");
assert.equal(historical.team_citations[0].personal_result_id, undefined);

const memberOptions = { headers: { token: "fixture-member", "X-Space-Id": "fixture-space" } };
const member = await request("/summaries/103/contents", memberOptions);
const memberContent = member.contents.find((content) => content.kind === "personal");
assert.equal(memberContent.current_version.content, "private member report");
await request(`/summaries/103/contents/${memberContent.content_id}/versions`, options, 403);
await request(`/summaries/103/contents/${memberContent.content_id}/versions/${versions[6].version_id}`, memberOptions, 404);
await request("/summaries/101/contents", {}, 401);
await request("/summaries/101/contents", { headers: { token: "fixture-owner", "X-Space-Id": "not-enabled" } }, 404);
await request("/summaries/101/contents", { headers: { token: "fixture-outsider", "X-Space-Id": "fixture-space" } }, 403);
console.log("PASS: image HTTP contract, 7-version pagination, read-only provisional V1, team privacy, namespace and Space/auth isolation");
