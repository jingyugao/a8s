import { createConnectTransport } from "@connectrpc/connect-node";
import { createA8sClient, type ClientOptions } from "./index.js";

export function createNodeA8sClient(baseUrl: string, options: ClientOptions = {}) {
  const url = new URL(baseUrl);
  if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error("endpoint must be an http(s) URL without credentials, query, or fragment");
  return createA8sClient(createConnectTransport({ baseUrl, httpVersion: "1.1" }), options);
}
