import { createHash, randomUUID } from 'node:crypto';
import path from 'node:path';
import { loadIntent, loadReference, saveIntent, saveReference } from './receipts.mjs';

const MAX_RESPONSE_BYTES = 1 << 20;
const MAX_PROMPT_BYTES = 64 << 10;
const STATUSES = new Set(['pending', 'running', 'success', 'error', 'interrupted', 'timeout']);
const TERMINAL = new Set(['success', 'error', 'interrupted', 'timeout']);

function canonical(value) {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']';
  if (value !== null && typeof value === 'object') {
    return '{' + Object.keys(value).sort().map((key) => JSON.stringify(key) + ':' + canonical(value[key])).join(',') + '}';
  }
  return JSON.stringify(value);
}
function hash(value) { return createHash('sha256').update(canonical(value)).digest('hex'); }
function object(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }
function boundedNumber(value, fallback, minimum, maximum) {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) throw new Error('provider_config_invalid');
  return value;
}
function configuredURL(value, allowInsecureLoopback) {
  let url;
  try { url = new URL(value); } catch { throw new Error('provider_url_invalid'); }
  const loopback = ['127.0.0.1', '[::1]'].includes(url.hostname);
  if (url.username || url.password || url.search || url.hash || (url.protocol !== 'https:' && !(url.protocol === 'http:' && loopback && allowInsecureLoopback === true))) {
    throw new Error('provider_url_invalid');
  }
  return url.href.replace(/\/+$/, '');
}
function abortable(promise, signal) {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const aborted = () => reject(signal.reason);
    signal.addEventListener('abort', aborted, { once: true });
    Promise.resolve(promise).then(resolve, reject).finally(() => signal.removeEventListener('abort', aborted));
  });
}
async function pause(ms, signal) {
  let timer;
  try { await abortable(new Promise((resolve) => { timer = setTimeout(resolve, ms); }), signal); }
  finally { clearTimeout(timer); }
}
async function boundedJSON(response, signal) {
  const claimed = response.headers.get('content-length');
  if (claimed && /^\d+$/.test(claimed) && Number(claimed) > MAX_RESPONSE_BYTES) {
    void response.body?.cancel().catch(() => {});
    throw new Error('response_too_large');
  }
  if (!response.body) throw new Error('response_invalid');
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { value, done } = await abortable(reader.read(), signal);
      if (done) break;
      size += value.byteLength;
      if (size > MAX_RESPONSE_BYTES) throw new Error('response_too_large');
      chunks.push(value);
    }
    const data = Buffer.concat(chunks, size);
    try { return JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(data)); }
    catch { throw new Error('response_invalid'); }
  } finally {
    void reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

function finalText(values) {
  const messages = object(values) && values.messages;
  if (!Array.isArray(messages) || messages.length === 0) throw new Error('text_output_unsupported');
  const message = messages.at(-1);
  if (!object(message)) throw new Error('text_output_unsupported');
  const data = message.data === undefined ? message : message.data;
  if (!object(data)) throw new Error('text_output_unsupported');
  let assistantFound = false;
  for (const source of [message, data]) {
    for (const key of ['role', 'type']) {
      if (source[key] === undefined) continue;
      if (!['ai', 'assistant', 'AIMessage'].includes(source[key])) throw new Error('text_output_unsupported');
      assistantFound = true;
    }
  }
  if (!assistantFound) throw new Error('text_output_unsupported');
  for (const source of [message, data, message.additional_kwargs, message.response_metadata, data.additional_kwargs, data.response_metadata]) {
    if (source == null) continue;
    if (!object(source)) throw new Error('text_output_unsupported');
    if (source.channel != null && source.channel !== 'final') throw new Error('text_output_unsupported');
    for (const key of ['tool_calls', 'invalid_tool_calls', 'toolCalls']) if (source[key] != null && (!Array.isArray(source[key]) || source[key].length !== 0)) throw new Error('text_output_unsupported');
    for (const key of ['function_call', 'functionCall', 'tool_call_id', 'reasoning_content', 'thinking', 'reasoning']) if (source[key] != null) throw new Error('text_output_unsupported');
    for (const key of ['finish_reason', 'finishReason', 'stop_reason']) if (source[key] != null && !['stop', 'end_turn', 'stop_sequence'].includes(source[key])) throw new Error('text_output_unsupported');
  }
  if (data !== message && message.content !== undefined) throw new Error('text_output_unsupported');
  const content = data.content;
  if (typeof content !== 'string' || !content.trim() || (values.result !== undefined && values.result !== content)) throw new Error('text_output_unsupported');
  return content;
}
function usageFields(values) {
  const usage = values?.usage;
  if (!object(usage) || usage.unmetered === true) return {};
  const prompt = usage.prompt_tokens, completion = usage.completion_tokens;
  const result = {};
  if ([prompt, completion].every((value) => Number.isSafeInteger(value) && value >= 0) && Number.isSafeInteger(prompt + completion) && (usage.total_tokens === undefined || usage.total_tokens === prompt + completion)) {
    result.tokenUsage = { prompt, completion, total: prompt + completion };
  }
  if (typeof usage.cost_usd === 'number' && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0) result.cost = usage.cost_usd;
  return result;
}

// Exact promptfoo 0.125.1 compatibility is qualified separately. These two
// fields prevent its scheduler from replaying a whole side-effecting callApi.
export default class RunkiteProvider {
  handlesOwnRetries = true;
  #apiKey;

  constructor(options = {}, dependencies = {}) {
    const config = options.config ?? {};
    const environment = { ...process.env, ...options.env };
    this.config = { maxRetries: 0 };
    this.providerId = options.id || 'runkite-managed';
    this.profile = config.profile || 'runkite-managed';
    this.baseUrl = configuredURL(config.baseUrl ?? environment.RUNKITE_URL, config.allowInsecureLoopback);
    this.agentId = config.agentId ?? environment.RUNKITE_AGENT_ID;
    const keyEnv = config.apiKeyEnv ?? 'RUNKITE_API_KEY';
    this.#apiKey = environment[keyEnv];
    this.directory = path.resolve(config.receiptDirectory ?? environment.RUNKITE_RECEIPT_DIR ?? path.join(config.basePath ?? process.cwd(), '.runkite-promptfoo-receipts'));
    this.timeoutMs = boundedNumber(config.timeoutMs, 60_000, 10, 300_000);
    this.requestTimeoutMs = boundedNumber(config.requestTimeoutMs, 10_000, 5, 30_000);
    this.pollIntervalMs = boundedNumber(config.pollIntervalMs, 100, 1, 5_000);
    this.outputMode = config.outputMode ?? 'text';
    if (typeof this.agentId !== 'string' || !this.agentId || this.agentId.length > 200 || typeof this.profile !== 'string' || !this.profile || this.profile.length > 100 || typeof this.#apiKey !== 'string' || !this.#apiKey || !['text', 'json'].includes(this.outputMode)) throw new Error('provider_config_invalid');
    this.fetcher = dependencies.fetch ?? globalThis.fetch;
    this.uuid = dependencies.randomUUID ?? randomUUID;
  }

  id() { return this.providerId; }

  deadline(options) {
    return AbortSignal.any([AbortSignal.timeout(this.timeoutMs), ...(options?.abortSignal ? [options.abortSignal] : [])]);
  }

  async request(method, suffix, body, signal, expectedReference) {
    const requestSignal = AbortSignal.any([signal, AbortSignal.timeout(this.requestTimeoutMs)]);
    const headers = { Authorization: `Bearer ${this.#apiKey}`, 'Content-Type': 'application/json' };
    if (expectedReference) headers['Runkite-Expected-Run-Reference'] = expectedReference;
    const response = await abortable(this.fetcher(`${this.baseUrl}${suffix}`, {
      method, headers, redirect: 'error', signal: requestSignal,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    }), requestSignal);
    if (!response.ok) {
      void response.body?.cancel().catch(() => {});
      const error = new Error(response.status === 404 ? 'retained_run_missing' : response.status === 412 ? 'run_reference_changed' : 'http_failure');
      error.httpStatus = response.status;
      throw error;
    }
    const data = await boundedJSON(response, requestSignal);
    const reference = response.headers.get('Runkite-Run-Reference') ?? undefined;
    if (expectedReference && reference !== expectedReference) throw new Error('run_reference_changed');
    return { data, reference };
  }

  verifyRun(run, receipt) {
    if (!object(run) || run.run_id !== receipt.run_id || run.thread_id !== receipt.thread_id || run.agent_id !== receipt.agent_id || !STATUSES.has(run.status) || !object(run.input) || hash(run.input) !== receipt.input_sha256) throw new Error('run_identity_changed');
  }

  metadata(receipt, receiptPath, reference, state) {
    return { runkite: { run_id: receipt.run_id, thread_id: receipt.thread_id, agent_id: receipt.agent_id, receipt: receiptPath, ...(state === 'unknown' ? {} : { run_reference_acquired: Boolean(reference) }), state } };
  }

  async observe(receipt, receiptPath, signal) {
    let reference = await abortable(loadReference(receiptPath), signal);
    while (!signal.aborted) {
      try {
        const observed = await this.request('GET', `/runs/${encodeURIComponent(receipt.run_id)}`, undefined, signal, reference);
        this.verifyRun(observed.data, receipt);
        if (!reference && observed.reference !== undefined) {
          await abortable(saveReference(receiptPath, observed.reference), signal);
          reference = observed.reference;
        }
        const waited = await this.request('GET', `/runs/${encodeURIComponent(receipt.run_id)}/wait`, undefined, signal, reference);
        this.verifyRun(waited.data?.run, receipt);
        if (!reference && waited.reference !== undefined) {
          await abortable(saveReference(receiptPath, waited.reference), signal);
          reference = waited.reference;
        }
        const run = waited.data.run;
        if (TERMINAL.has(run.status)) {
          if (run.status !== 'success') return { error: `Runkite managed run ended with ${run.status}`, metadata: this.metadata(receipt, receiptPath, reference, run.status) };
          const values = waited.data.values;
          if (!object(values)) throw new Error('output_missing');
          return { output: this.outputMode === 'json' ? values : finalText(values), cached: false, ...usageFields(values), metadata: this.metadata(receipt, receiptPath, reference, 'success') };
        }
      } catch (error) {
        if (['retained_run_missing', 'run_identity_changed', 'run_reference_changed', 'run_reference_invalid', 'response_too_large', 'response_invalid', 'output_missing', 'text_output_unsupported'].includes(error.message) || [400, 401, 403, 409, 412, 428].includes(error.httpStatus)) throw error;
        // Only read operations retry. A timed-out wait does not authorize a
        // new create or establish that the managed worker stopped.
      }
      await pause(this.pollIntervalMs, signal);
    }
    signal.throwIfAborted();
  }

  async execute(receipt, receiptPath, signal, createInput) {
    let creationAttempted = false;
    try {
      signal.throwIfAborted();
      if (createInput !== undefined) {
        creationAttempted = true;
        let createdReference;
        try {
          const created = await this.request('POST', `/threads/${encodeURIComponent(receipt.thread_id)}/runs`, {
            run_id: receipt.run_id, agent_id: receipt.agent_id, input: createInput, if_not_exists: 'create', multitask_strategy: 'reject',
          }, signal);
          this.verifyRun(created.data, receipt);
          createdReference = created.reference;
        } catch (error) {
          if (error.message === 'run_identity_changed') throw error;
          // Delivery may have committed. Do not issue another POST, even if
          // the subsequent read cannot yet see a run row.
        }
        // Preserve identity evidence from any verified create response too.
        // A failed durable pin must not fall through as a lost delivery.
        if (createdReference !== undefined) await abortable(saveReference(receiptPath, createdReference), signal);
      }
      return await this.observe(receipt, receiptPath, signal);
    } catch {
      return {
        error: 'Runkite outcome is unknown or unavailable. Inspect the saved handle; recovery is read-only. Observation cancellation does not prove worker cancellation.',
        metadata: { ...this.metadata(receipt, receiptPath, undefined, 'unknown'), creation_attempted: creationAttempted },
      };
    }
  }

  async callApi(prompt, _context, options) {
    if (typeof prompt !== 'string' || Buffer.byteLength(prompt) > MAX_PROMPT_BYTES) return { error: 'Runkite provider requires one text prompt of at most 64 KiB' };
    if (options?.abortSignal?.aborted) return { error: 'Runkite observation was cancelled before dispatch' };
    const signal = this.deadline(options);
    const input = { messages: [{ role: 'user', content: prompt }] };
    const receipt = { version: 1, endpoint: this.baseUrl, profile: this.profile, agent_id: this.agentId, run_id: `pfoo-${this.uuid()}`, thread_id: `pfoo-${this.uuid()}`, input_sha256: hash(input) };
    let receiptPath;
    try { receiptPath = await abortable(saveIntent(this.directory, receipt), signal); }
    catch { return { error: 'Runkite intent could not be durably saved; no create was sent' }; }
    return this.execute(receipt, receiptPath, signal, input);
  }

  async observeReceipt(receiptPath, options) {
    const signal = this.deadline(options);
    try {
      const receipt = await abortable(loadIntent(receiptPath), signal);
      if (receipt.endpoint !== this.baseUrl || receipt.profile !== this.profile || receipt.agent_id !== this.agentId || !/^[a-f0-9]{64}$/.test(receipt.input_sha256) || typeof receipt.thread_id !== 'string') throw new Error('receipt_binding_changed');
      return this.execute(receipt, path.resolve(receiptPath), signal);
    } catch { return { error: 'Runkite receipt or configured destination does not match; no create was sent' }; }
  }
}
