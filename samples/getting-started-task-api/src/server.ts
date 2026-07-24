import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";

export interface Task {
  id: number;
  title: string;
  completed: boolean;
}

const initialTasks: readonly Task[] = [
  { id: 1, title: "Read the getting-started guide", completed: true },
  { id: 2, title: "Open the first autonomous pull request", completed: false }
];

class RequestError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

function sendJSON(response: ServerResponse, status: number, value: unknown): void {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "content-length": Buffer.byteLength(body),
    "content-type": "application/json"
  });
  response.end(body);
}

async function readObject(request: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  for await (const chunk of request) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }

  try {
    const value: unknown = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    if (value === null || typeof value !== "object" || Array.isArray(value)) {
      throw new RequestError(400, "request body must be a JSON object");
    }
    return value as Record<string, unknown>;
  } catch (error) {
    if (error instanceof RequestError) {
      throw error;
    }
    if (error instanceof SyntaxError) {
      throw new RequestError(400, "request body must be valid JSON");
    }
    throw error;
  }
}

async function handleRequest(
  request: IncomingMessage,
  response: ServerResponse,
  tasks: Task[],
  allocateID: () => number
): Promise<void> {
  const url = new URL(request.url ?? "/", "http://localhost");

  if (request.method === "GET" && url.pathname === "/health") {
    sendJSON(response, 200, { status: "ok" });
    return;
  }

  if (request.method === "GET" && url.pathname === "/tasks") {
    sendJSON(response, 200, { tasks });
    return;
  }

  if (request.method === "POST" && url.pathname === "/tasks") {
    const input = await readObject(request);
    const task: Task = {
      id: allocateID(),
      title: typeof input.title === "string" ? input.title : "",
      completed: false
    };
    tasks.push(task);
    sendJSON(response, 201, task);
    return;
  }

  const completion = /^\/tasks\/(\d+)\/complete$/.exec(url.pathname);
  if (request.method === "PATCH" && completion !== null) {
    const id = Number(completion[1]);
    const task = tasks.find((candidate) => candidate.id === id);
    if (task === undefined) {
      sendJSON(response, 404, { error: "task not found" });
      return;
    }
    task.completed = !task.completed;
    sendJSON(response, 200, task);
    return;
  }

  sendJSON(response, 404, { error: "not found" });
}

export function createTaskServer(seedTasks: readonly Task[] = initialTasks): Server {
  const tasks = seedTasks.map((task) => ({ ...task }));
  let nextID = tasks.reduce((highest, task) => Math.max(highest, task.id), 0) + 1;

  return createServer((request, response) => {
    void handleRequest(request, response, tasks, () => nextID++).catch((error: unknown) => {
      if (error instanceof RequestError) {
        sendJSON(response, error.status, { error: error.message });
        return;
      }
      console.error(error);
      sendJSON(response, 500, { error: "internal server error" });
    });
  });
}
