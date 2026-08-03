/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export declare const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export declare function currentTenant(): string;
export declare function runWithTenant<T>(tenantId: string | undefined, fn: () => Promise<T>): Promise<T>;
export declare function tenantHeaders(): Record<string, string>;
