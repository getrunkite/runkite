/** Walk before stringify: custom toJSON/getters must never change native state. */
export declare function stateJson(value: unknown): string;
export type StateSnapshot<T> = {
    value: T | null;
    etag: string | null;
};
export declare class AiSdkState {
    private readonly baseUrl;
    private readonly runnerKind;
    private readonly runnerToken?;
    private readonly request;
    constructor(baseUrl: string, runnerKind: string, runnerToken?: string | undefined, request?: typeof fetch);
    private url;
    private headers;
    load<T>(threadId: string, signal?: AbortSignal): Promise<StateSnapshot<T>>;
    save(threadId: string, value: unknown, etag: string | null): Promise<void>;
}
