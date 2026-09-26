/**
 * Entire Brain — TypeScript client.
 *
 * A brain runs on your machine and, until now, could only be reached by a
 * program willing to speak JSON-RPC or shell out to the CLI and parse its
 * output. This is the small amount of code that makes it an ordinary HTTP
 * service instead.
 *
 *   import { Brain } from "./entire-brain";
 *
 *   const brain = new Brain("http://127.0.0.1:7777", process.env.BRAIN_TOKEN!);
 *   for (const fact of await brain.facts()) console.log(fact.text);
 *   for (const hit of await brain.search("retry policy")) console.log(hit.source, hit.text);
 *
 * Start the server with:
 *
 *   entire brain mcp --http 7777
 *
 * It binds loopback and prints a bearer token. Nothing here works without one.
 *
 * No dependencies: global fetch only. A memory client that drags in a
 * dependency tree is a client people vendor rather than install.
 */

export interface Fact {
  id: string;
  text: string;
  kind?: string;
  paths?: string[];
  status?: string;
  origin?: string;
  created_at?: string;
  updated_at?: string;
}

export interface SearchHit {
  source: string;
  id: string;
  text: string;
  path?: string;
  heading?: string;
  score?: number;
}

export interface BrainStatus {
  branch: string;
  facts: { active: number; superseded: number; retracted: number; total: number };
}

/**
 * A request to a brain failed. Carries the status and the server's own message,
 * because "it broke" is not an error report. A 401 almost always means the
 * token is missing or stale, and saying so beats making the caller guess.
 */
export class BrainError extends Error {
  constructor(
    readonly status: number,
    readonly detail: string,
    readonly hint?: string,
  ) {
    let message = `brain request failed (${status}): ${detail}`;
    if (hint) message += `\n  ${hint}`;
    if (status === 401) {
      message += "\n  Check the bearer token printed by `entire brain mcp --http`.";
    }
    super(message);
    this.name = "BrainError";
  }
}

export class Brain {
  private readonly baseUrl: string;

  constructor(
    baseUrl: string,
    private readonly token: string,
    private readonly timeoutMs = 30_000,
  ) {
    if (!token) {
      // Failing in the constructor rather than at the first request: an empty
      // token is a configuration mistake, and a 401 three calls later is a
      // worse way to find out about it.
      throw new Error("a bearer token is required; `entire brain mcp --http` prints one");
    }
    this.baseUrl = baseUrl.replace(/\/+$/, "");
  }

  private async get<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
    const url = new URL(this.baseUrl + path);
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== null) url.searchParams.set(key, String(value));
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    let response: Response;
    try {
      response = await fetch(url, {
        headers: { Authorization: `Bearer ${this.token}` },
        signal: controller.signal,
      });
    } catch (cause) {
      throw new BrainError(0, `could not reach ${this.baseUrl}: ${String(cause)}`);
    } finally {
      clearTimeout(timer);
    }
    const body = await response.text();
    if (!response.ok) {
      try {
        const parsed = JSON.parse(body) as { error?: string; hint?: string };
        throw new BrainError(response.status, parsed.error ?? body, parsed.hint);
      } catch (error) {
        if (error instanceof BrainError) throw error;
        throw new BrainError(response.status, body.trim() || response.statusText);
      }
    }
    return JSON.parse(body) as T;
  }

  /** Fact counts for a branch: active, superseded, retracted, total. */
  status(branch?: string): Promise<BrainStatus> {
    return this.get<BrainStatus>("/v1/status", { branch });
  }

  /**
   * Durable facts. Superseded and retracted facts are excluded unless
   * includeAll is set: a caller rendering these should not be shown things the
   * brain no longer believes.
   */
  async facts(options: { limit?: number; branch?: string; includeAll?: boolean } = {}): Promise<Fact[]> {
    const payload = await this.get<{ facts: Fact[] }>("/v1/facts", {
      limit: options.limit ?? 20,
      branch: options.branch,
      all: options.includeAll ? "true" : undefined,
    });
    return payload.facts ?? [];
  }

  /**
   * Hybrid retrieval across facts, history and docs. `source` narrows the
   * corpus ("fact" | "history" | "doc"); omitting it searches the default set.
   */
  async search(
    query: string,
    options: { limit?: number; source?: string; branch?: string } = {},
  ): Promise<SearchHit[]> {
    if (!query.trim()) throw new Error("query must not be empty");
    const payload = await this.get<{ results: SearchHit[] }>("/v1/search", {
      q: query,
      limit: options.limit ?? 20,
      source: options.source,
      branch: options.branch,
    });
    return payload.results ?? [];
  }

  /** What this server exposes. Useful when a client is newer than a brain. */
  async endpoints(): Promise<Array<{ method: string; path: string; description: string }>> {
    const payload = await this.get<{ endpoints: Array<{ method: string; path: string; description: string }> }>("/v1/");
    return payload.endpoints ?? [];
  }
}
