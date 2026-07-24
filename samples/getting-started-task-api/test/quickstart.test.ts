import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { access, cp, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, relative, sep } from "node:path";
import { promisify } from "node:util";
import { describe, it } from "node:test";

const execute = promisify(execFile);
const proofIssueID = "reject-empty-task-titles";

interface SeedIssue {
  id: string;
  title: string;
  body: string;
}

interface SeedCatalog {
  sample: {
    id: string;
    version: string;
  };
  issues: SeedIssue[];
}

interface PullRequest {
  number: number;
  title: string;
  body: string;
  base: string;
  head: string;
  headCommit: string;
}

interface QuickstartProof {
  sampleVersion: string;
  issueID: string;
  stages: string[];
  pullRequest: PullRequest;
  disposableRoot: string;
}

async function run(cwd: string, command: string, args: string[]): Promise<string> {
  const result = await execute(command, args, {
    cwd,
    encoding: "utf8"
  });
  return result.stdout;
}

async function copySample(sampleRoot: string, worktree: string): Promise<void> {
  const excluded = new Set([".git", "dist", "node_modules"]);
  await cp(sampleRoot, worktree, {
    recursive: true,
    filter: (source) => {
      const path = relative(sampleRoot, source);
      return path === "" || !path.split(sep).some((part) => excluded.has(part));
    }
  });
  await symlink(
    join(sampleRoot, "node_modules"),
    join(worktree, "node_modules"),
    process.platform === "win32" ? "junction" : "dir"
  );
}

async function initializeTarget(root: string, sampleRoot: string): Promise<{ worktree: string; remote: string }> {
  const worktree = join(root, "target");
  const remote = join(root, "target.git");
  await copySample(sampleRoot, worktree);
  await run(worktree, "git", ["init", "--initial-branch=main"]);
  await run(worktree, "git", ["config", "user.email", "quickstart@example.invalid"]);
  await run(worktree, "git", ["config", "user.name", "Quickstart Fixture"]);
  await run(worktree, "git", ["add", "-A"]);
  await run(worktree, "git", ["commit", "-m", "seed versioned tutorial target"]);
  await run(root, "git", ["clone", "--bare", worktree, remote]);
  await run(worktree, "git", ["remote", "add", "origin", remote]);
  return { worktree, remote };
}

async function implementRequiredTitle(worktree: string): Promise<void> {
  const serverPath = join(worktree, "src", "server.ts");
  const server = await readFile(serverPath, "utf8");
  const vulnerableCreation = `    const input = await readObject(request);
    const task: Task = {
      id: allocateID(),
      title: typeof input.title === "string" ? input.title : "",
      completed: false
    };`;
  const validatedCreation = `    const input = await readObject(request);
    const title = typeof input.title === "string" ? input.title.trim() : "";
    if (title === "") {
      sendJSON(response, 400, { error: "title is required" });
      return;
    }
    const task: Task = {
      id: allocateID(),
      title,
      completed: false
    };`;
  assert.ok(server.includes(vulnerableCreation), "seed issue is not present in the pinned sample");
  await writeFile(serverPath, server.replace(vulnerableCreation, validatedCreation));

  const testPath = join(worktree, "test", "server.test.ts");
  const test = await readFile(testPath, "utf8");
  const end = test.lastIndexOf("\n});");
  assert.notEqual(end, -1, "server test suite has no closing block");
  const regression = `

  it("rejects invalid titles", async () => {
    for (const title of [undefined, 42, "   "]) {
      const response = await fetch(\`\${baseURL}/tasks\`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ title })
      });

      assert.equal(response.status, 400);
      assert.deepEqual(await response.json(), { error: "title is required" });
    }
  });

  it("trims a valid title", async () => {
    const response = await fetch(\`\${baseURL}/tasks\`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ title: "  Watch the workflow  " })
    });

    assert.equal(response.status, 201);
    assert.equal((await response.json()).title, "Watch the workflow");
  });`;
  await writeFile(testPath, `${test.slice(0, end)}${regression}${test.slice(end)}`);

  await run(worktree, "git", ["add", "src/server.ts", "test/server.test.ts"]);
  await run(worktree, "git", ["commit", "-m", "fix: reject empty task titles"]);
}

async function reviewImplementation(worktree: string): Promise<void> {
  const changed = (await run(worktree, "git", ["diff", "--name-only", "main...HEAD"]))
    .trim()
    .split("\n")
    .filter(Boolean);
  assert.deepEqual(changed, ["src/server.ts", "test/server.test.ts"]);

  const diff = await run(worktree, "git", ["diff", "main...HEAD"]);
  for (const expected of ["input.title.trim()", "title is required", "rejects invalid titles", "trims a valid title"]) {
    assert.match(diff, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  }
}

async function runFocusedCI(worktree: string): Promise<void> {
  await run(worktree, "npm", ["run", "build"]);
  await run(worktree, process.execPath, ["--test", "dist/test/server.test.js"]);
}

async function openPullRequest(
  root: string,
  remote: string,
  issue: SeedIssue,
  head: string
): Promise<PullRequest> {
  const headCommit = (await run(root, "git", ["--git-dir", remote, "rev-parse", `refs/heads/${head}`])).trim();
  assert.notEqual(headCommit, "");
  return {
    number: 1,
    title: issue.title,
    body: `Resolves seeded issue \`${issue.id}\`.`,
    base: "main",
    head,
    headCommit
  };
}

async function runQuickstartProof(sampleRoot: string): Promise<QuickstartProof> {
  const root = await mkdtemp(join(tmpdir(), "goobers-quickstart-"));
  const stages: string[] = [];
  try {
    const catalog: SeedCatalog = JSON.parse(await readFile(join(sampleRoot, "seed-issues.json"), "utf8"));
    const issue = catalog.issues.find((candidate) => candidate.id === proofIssueID);
    assert.ok(issue, `seed issue ${proofIssueID} is missing`);

    const { worktree, remote } = await initializeTarget(root, sampleRoot);
    stages.push("query-backlog");

    const branch = "goobers/quickstart/first-pr";
    await run(worktree, "git", ["switch", "-c", branch]);
    await implementRequiredTitle(worktree);
    stages.push("implement");

    await reviewImplementation(worktree);
    stages.push("review");

    await runFocusedCI(worktree);
    stages.push("local-ci");

    await run(worktree, "git", ["push", "--set-upstream", "origin", branch]);
    stages.push("push-branch");

    const pullRequest = await openPullRequest(root, remote, issue, branch);
    stages.push("open-pr");

    return {
      sampleVersion: catalog.sample.version,
      issueID: issue.id,
      stages,
      pullRequest,
      disposableRoot: root
    };
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return false;
    }
    throw error;
  }
}

describe("quickstart fixture proof", () => {
  it("turns the first seeded issue into a reviewed pull request and tears down the target", async () => {
    const proof = await runQuickstartProof(process.cwd());

    assert.equal(proof.sampleVersion, "1.0.0");
    assert.equal(proof.issueID, proofIssueID);
    assert.deepEqual(proof.stages, [
      "query-backlog",
      "implement",
      "review",
      "local-ci",
      "push-branch",
      "open-pr"
    ]);
    assert.equal(proof.pullRequest.number, 1);
    assert.equal(proof.pullRequest.title, "Reject tasks with empty titles");
    assert.equal(proof.pullRequest.base, "main");
    assert.equal(proof.pullRequest.head, "goobers/quickstart/first-pr");
    assert.match(proof.pullRequest.headCommit, /^[0-9a-f]{40,64}$/);
    assert.equal(await pathExists(proof.disposableRoot), false);
  });
});
