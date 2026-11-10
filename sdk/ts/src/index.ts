import type { MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import {
  ControlService, ErrorCategory, ErrorDetailSchema, GetOperationRequestSchema,
  OperationStatus, type ErrorDetail, type Operation,
} from "./gen/a8s/v1/control_pb.js";
import { ExecutionService } from "./gen/a8s/execution/v1/execution_pb.js";

export * from "./gen/a8s/v1/control_pb.js";
export * as execution from "./gen/a8s/execution/v1/execution_pb.js";
export * as agentc from "./gen/a8s/agentc/v1/agentc_pb.js";

export interface ClientOptions {
  headers?: HeadersInit;
  requestTimeoutMs?: number;
  pollIntervalMs?: number;
}

export class RequestError extends Error {
  readonly code: Code;
  readonly category?: string;
  readonly source?: string;
  readonly nativeCode?: string;
  readonly detail?: ErrorDetail;
  readonly requestKey?: string;
  readonly requestId?: string;
  readonly resourceId?: string;
  readonly operationId?: string;
  readonly outcomeUnknown: boolean;

  constructor(cause: unknown, input: unknown, mutation = false) {
    const error = ConnectError.from(cause);
    const detail = error.findDetails(ErrorDetailSchema)[0];
    super(detail?.message ?? error.rawMessage, { cause });
    this.name = "RequestError";
    this.code = error.code;
    this.category = error.metadata.get("a8s-error-code") ?? (detail ? ErrorCategory[detail.category] : undefined);
    this.source = error.metadata.get("a8s-error-source") ?? detail?.source;
    this.nativeCode = error.metadata.get("a8s-native-code") ?? detail?.code;
    this.detail = detail;
    const request = input as { requestKey?: string; id?: string; requestId?: string; agentc?: { id?: string }; runtimeRevision?: { id?: string } };
    this.requestKey = request.requestKey;
    this.requestId = error.metadata.get("a8s-request-id") ?? request.requestId;
    this.resourceId = request.id ?? request.agentc?.id ?? request.runtimeRevision?.id;
    this.operationId = detail?.operationId;
    this.outcomeUnknown = detail?.category === ErrorCategory.OUTCOME_UNKNOWN || this.category === "OutcomeUnknown" ||
      (mutation && [Code.DeadlineExceeded, Code.Canceled, Code.Unavailable, Code.Unknown].includes(error.code));
  }
}

export class OperationError extends Error {
  constructor(readonly operation: Operation) {
    super(`operation ${operation.id}: ${OperationStatus[operation.status]}: ${operation.error?.message ?? ""}`);
    this.name = "OperationError";
  }
}

export type OperationQuery = MessageInitShape<typeof GetOperationRequestSchema>;
export class WaitError extends Error {
  constructor(readonly request: OperationQuery, readonly operation: Operation | undefined, cause: unknown) {
    super(`stopped waiting for operation ${request.id}; server outcome is unchanged`, { cause });
    this.name = "WaitError";
  }
}

function positive(value: number, name: string): number {
  if (!Number.isSafeInteger(value) || value <= 0 || value > 2_147_483_647) throw new Error(`${name} must be a positive bounded integer`);
  return value;
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return; }
    const abort = () => { clearTimeout(timer); reject(signal.reason); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, ms);
    signal.addEventListener("abort", abort, { once: true });
  });
}

// Transport and identity belong to the application. This wrapper bounds unary
// calls and preserves request keys; it never retries a mutation or creates keys.
export function createA8sClient(transport: Transport, options: ClientOptions = {}) {
  const timeout = positive(options.requestTimeoutMs ?? 30_000, "requestTimeoutMs");
  const interval = positive(options.pollIntervalMs ?? 1_000, "pollIntervalMs");
  const identity = new Headers(options.headers);
  const mutations = new Set(["RegisterAgentc", "CreateRuntimeRevision", "CreateDeployment", "UpdateDeployment", "SetDesiredState", "Rollback", "OpenThread", "SubmitInput", "Cancel", "RespondInteraction"]);
  function headers(input: HeadersInit | undefined): Headers {
    const result = new Headers(identity);
    new Headers(input).forEach((value, name) => result.set(name, value));
    return result;
  }
  const bounded: Transport = {
    async unary(method, signal, timeoutMs, header, input, contextValues) {
      try {
        return await transport.unary(method, signal, Math.min(timeoutMs ?? timeout, timeout), headers(header), input, contextValues);
      } catch (error) { throw new RequestError(error, input, mutations.has(method.name)); }
    },
    stream(method, signal, timeoutMs, header, input, contextValues) {
      return transport.stream(method, signal, timeoutMs, headers(header), input, contextValues);
    },
  };
  const control = createClient(ControlService, bounded);
  const execution = createClient(ExecutionService, bounded);

  async function waitOperation(request: OperationQuery, wait: { timeoutMs?: number; signal?: AbortSignal } = {}): Promise<Operation> {
    if (!request.id) throw new Error("operation ID is required");
    const timeoutMs = positive(wait.timeoutMs ?? 120_000, "wait timeoutMs");
    const controller = new AbortController();
    const abort = () => controller.abort(wait.signal?.reason);
    if (wait.signal?.aborted) abort();
    else wait.signal?.addEventListener("abort", abort, { once: true });
    const timer = setTimeout(() => controller.abort(new Error("operation wait deadline exceeded")), timeoutMs);
    let latest: Operation | undefined;
    try {
      for (;;) {
        const response = await control.getOperation(request, { signal: controller.signal });
        latest = response.operation;
        if (!latest) throw new Error("server returned no operation");
        if (latest.status === OperationStatus.SUCCEEDED) return latest;
        if ([OperationStatus.FAILED, OperationStatus.BLOCKED, OperationStatus.UNKNOWN].includes(latest.status)) throw new OperationError(latest);
        if (![OperationStatus.PENDING, OperationStatus.RUNNING].includes(latest.status)) throw new Error("unrecognized operation status");
        await delay(interval, controller.signal);
      }
    } catch (error) {
      if (error instanceof OperationError) throw error;
      throw new WaitError(request, latest, error);
    } finally {
      clearTimeout(timer);
      wait.signal?.removeEventListener("abort", abort);
    }
  }
  return { control, execution, waitOperation };
}

export type A8sClient = ReturnType<typeof createA8sClient>;
