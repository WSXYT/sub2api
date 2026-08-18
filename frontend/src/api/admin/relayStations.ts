import { apiClient } from "../client";

export type RelayStationType = "aihub" | "newapi" | "sub2api";
export type RelayRateStatus =
	| "ready"
	| "unauthenticated"
	| "unavailable"
	| "stale"
	| "";

export interface RelayStationCredentials {
	ui_password: boolean;
	proxy_token: boolean;
	username: boolean;
	password: boolean;
}

export interface RelayStation {
	id: string;
	type: RelayStationType;
	name: string;
	base_url: string;
	control_url: string;
	enabled: boolean;
	credentials: RelayStationCredentials;
	balance?: number | null;
	created_at: string;
	updated_at: string;
}

export interface RelayStationCreateInput {
	type: RelayStationType;
	name: string;
	base_url?: string;
	control_url?: string;
	ui_password?: string;
	proxy_token?: string;
	username: string;
	password: string;
	enabled: boolean;
}

export interface RelayStationUpdateInput {
	name?: string;
	base_url?: string;
	control_url?: string;
	ui_password?: string;
	proxy_token?: string;
	username?: string;
	password?: string;
	enabled?: boolean;
}

export interface RelayPriceBand {
	min?: number;
	max?: number;
}

export interface RelayStationSource {
	station_id: string;
	enabled: boolean;
	source_group: string;
	priority: number;
	delta: number;
	max_rate?: number | null;
	mode?: string;
	price_band?: RelayPriceBand;
}

export interface RelayAccount {
	station_id: string;
	station_name: string;
	station_type: RelayStationType;
	group_id: number;
	group_name: string;
	source_group: string;
	enabled: boolean;
	priority: number;
	rate_status: RelayRateStatus;
	effective_rate?: number | null;
	balance?: number | null;
}

export interface RelayGroupBinding {
	group_id: number;
	sources: RelayStationSource[];
}

export interface RelayStationGroup {
	name: string;
}

export interface RelayRate {
	station_id: string;
	station_name: string;
	source_group: string;
	status: RelayRateStatus;
	rate: number | null;
	effective_rate?: number | null;
	updated_at: string;
}

export interface RelayProfitEstimate {
	group_id: number;
	group_name: string;
	station_id: string;
	station_name: string;
	source_group: string;
	rate_status: RelayRateStatus;
	requests: number;
	total_cost: number;
	downstream_rate: number;
	upstream_rate?: number | null;
	estimated_revenue?: number | null;
	estimated_cost?: number | null;
	estimated_profit?: number | null;
}

interface StationMutationResult {
	station: RelayStation;
	aihub_synced: boolean;
}

interface BindingMutationResult {
	bindings: RelayGroupBinding[];
	aihub_synced: boolean;
}

const secretFields = [
	"ui_password",
	"proxy_token",
	"username",
	"password",
] as const;

export function effectiveRelayRate(
	rate: RelayRate | undefined,
	delta: number,
	maxRate?: number | null,
): number | null {
	if (
		rate?.status !== "ready" ||
		typeof rate.rate !== "number" ||
		!Number.isFinite(rate.rate)
	) {
		return null;
	}
	if (maxRate != null && rate.rate > maxRate) return null;
	const value = Math.min(rate.rate + Number(delta), maxRate ?? Number.POSITIVE_INFINITY);
	return Number.isFinite(value) && value >= 0 ? value : null;
}

export function relayErrorReason(error: unknown): string | undefined {
	if (!error || typeof error !== "object" || !("reason" in error))
		return undefined;
	const reason = (error as { reason?: unknown }).reason;
	return typeof reason === "string" ? reason : undefined;
}

export async function listStations(): Promise<RelayStation[]> {
	const { data } = await apiClient.get<RelayStation[]>("/admin/relay-stations");
	return data;
}

export async function createStation(
	input: RelayStationCreateInput,
): Promise<StationMutationResult> {
	const { data } = await apiClient.post<StationMutationResult>(
		"/admin/relay-stations",
		input,
	);
	return data;
}

export async function updateStation(
	id: string,
	input: RelayStationUpdateInput,
): Promise<StationMutationResult> {
	const payload = { ...input };
	for (const field of secretFields) {
		const value = payload[field];
		if (typeof value !== "string") continue;
		const trimmed = value.trim();
		if (trimmed) payload[field] = trimmed;
		else delete payload[field];
	}
	const { data } = await apiClient.put<StationMutationResult>(
		`/admin/relay-stations/${id}`,
		payload,
	);
	return data;
}

export async function deleteStation(
	id: string,
): Promise<{ aihub_synced: boolean }> {
	const { data } = await apiClient.delete<{ aihub_synced: boolean }>(
		`/admin/relay-stations/${id}`,
	);
	return data;
}

export async function testStation(id: string): Promise<{ rates: RelayRate[] }> {
	const { data } = await apiClient.post<{ rates: RelayRate[] }>(
		`/admin/relay-stations/${id}/test`,
	);
	return data;
}

export async function listRelayAccounts(): Promise<RelayAccount[]> {
	const { data } = await apiClient.get<{ accounts: RelayAccount[] }>(
		"/admin/relay-stations/accounts",
	);
	return data.accounts;
}

export async function updateRelayAccount(
	stationId: string,
	input: {
		group_id: number;
		source_group: string;
		enabled?: boolean;
		priority?: number;
	},
): Promise<void> {
	await apiClient.patch(`/admin/relay-stations/accounts/${stationId}`, input);
}

export async function listBindings(): Promise<{
	bindings: RelayGroupBinding[];
}> {
	const { data } = await apiClient.get<{ bindings: RelayGroupBinding[] }>(
		"/admin/relay-stations/bindings",
	);
	return data;
}

export async function updateBindings(
	bindings: RelayGroupBinding[],
): Promise<BindingMutationResult> {
	const { data } = await apiClient.put<BindingMutationResult>(
		"/admin/relay-stations/bindings",
		{
			bindings,
		},
	);
	return data;
}

export async function listGroups(
	stationId: string,
): Promise<{ groups: RelayStationGroup[] }> {
	const { data } = await apiClient.get<{ groups: RelayStationGroup[] }>(
		`/admin/relay-stations/${stationId}/groups`,
	);
	return data;
}

export async function listRates(
	stationId?: string,
): Promise<{ rates: RelayRate[] }> {
	const { data } = await apiClient.get<{ rates: RelayRate[] }>(
		"/admin/relay-stations/rates",
		{
			params: stationId ? { station_id: stationId } : undefined,
		},
	);
	return data;
}

export async function refreshRates(): Promise<{
	rates: RelayRate[];
	refreshed: boolean;
}> {
	const { data } = await apiClient.post<{
		rates: RelayRate[];
		refreshed: boolean;
	}>("/admin/relay-stations/rates/refresh");
	return data;
}

export async function getProfit(
	startAt: string,
	endAt: string,
): Promise<{
	start_at: string;
	end_at: string;
	estimates: RelayProfitEstimate[];
}> {
	const { data } = await apiClient.get<{
		start_at: string;
		end_at: string;
		estimates: RelayProfitEstimate[];
	}>("/admin/relay-stations/profit", {
		params: { start_at: startAt, end_at: endAt },
	});
	return data;
}

export const relayStationsAPI = {
	list: listStations,
	create: createStation,
	update: updateStation,
	delete: deleteStation,
	test: testStation,
	listRelayAccounts,
	updateRelayAccount,
	listBindings,
	updateBindings,
	listGroups,
	listRates,
	refreshRates,
	getProfit,
};

export default relayStationsAPI;
