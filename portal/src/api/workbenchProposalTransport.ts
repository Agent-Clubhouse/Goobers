import { MalformedResponseError } from "./errors";

/** Counts streamed response bytes before decoding. Source text is never rendered as HTML. */
export async function readMetadataResponse<T>(response: Response, limit: number): Promise<T> {
  if (!response.body) throw new MalformedResponseError("The source proposal response is empty.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const pieces: string[] = [];
  let bytes = 0;
  try {
    for (;;) {
      const part = await reader.read();
      if (part.done) break;
      bytes += part.value.byteLength;
      if (bytes > limit) {
        throw new Error("Source proposal response exceeds its byte bound.");
      }
      pieces.push(decoder.decode(part.value, { stream: true }));
    }
    pieces.push(decoder.decode());
    return JSON.parse(pieces.join("")) as T;
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw new MalformedResponseError("The source proposal response is malformed or exceeds its byte bound.", { cause: error });
  } finally {
    reader.releaseLock();
  }
}
