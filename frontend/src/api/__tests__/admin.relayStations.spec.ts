import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, patch, put } = vi.hoisted(() => ({
	get: vi.fn(),
	patch: vi.fn(),
	put: vi.fn(),
}));

vi.mock("@/api/client", () => ({ apiClient: { get, patch, put } }));

import {
	effectiveRelayRate,
	listRelayAccounts,
	relayErrorReason,
	updateRelayAccount,
	updateBindings,
	updateStation,
	type RelayRate,
} from "@/api/admin/relayStations";

const readyRate: RelayRate = {
	station_id: "station-1",
	station_name: "Primary",
	source_group: "default",
	status: "ready",
	rate: 0.8,
	updated_at: "2026-01-01T00:00:00Z",
};

describe("admin relay stations API", () => {
	beforeEach(() => {
		get.mockReset();
		patch.mockReset();
		put.mockReset();
		put.mockResolvedValue({ data: { station: {}, aihub_synced: true } });
	});

	it("does not send blank credentials when editing a station", async () => {
		await updateStation("station-1", {
			name: "Primary",
			ui_password: " ",
			proxy_token: " new-token ",
			username: "",
			password: undefined,
		});

		expect(put).toHaveBeenCalledWith("/admin/relay-stations/station-1", {
			name: "Primary",
			proxy_token: "new-token",
		});
	});

	it("returns an AIHub synchronization failure with saved bindings", async () => {
		put.mockResolvedValue({
			data: {
				bindings: [],
				aihub_synced: false,
				aihub_sync_error: "AIHub: connection refused",
			},
		});

		await expect(updateBindings([])).resolves.toEqual({
			bindings: [],
			aihub_synced: false,
			aihub_sync_error: "AIHub: connection refused",
		});
		expect(put).toHaveBeenCalledWith("/admin/relay-stations/bindings", { bindings: [] });
	});

	it("lists and updates relay accounts through the relay API", async () => {
		get.mockResolvedValue({ data: { accounts: [{ station_id: "station-1" }] } });
		patch.mockResolvedValue({ data: { updated: true } });

		await expect(listRelayAccounts()).resolves.toEqual([
			{ station_id: "station-1" },
		]);
		await updateRelayAccount("station-1", {
			group_id: 7,
			source_group: "default",
			priority: 100,
		});

		expect(get).toHaveBeenCalledWith("/admin/relay-stations/accounts");
		expect(patch).toHaveBeenCalledWith(
			"/admin/relay-stations/accounts/station-1",
			{
				group_id: 7,
				source_group: "default",
				priority: 100,
			},
		);
	});

	it("calculates only usable non-negative effective rates", () => {
		expect(effectiveRelayRate(readyRate, -0.1)).toBeCloseTo(0.7);
		expect(effectiveRelayRate({ ...readyRate, status: "stale" }, 0)).toBeNull();
		expect(effectiveRelayRate(readyRate, -1)).toBeNull();
		expect(
			effectiveRelayRate({ ...readyRate, rate: 0.09 }, 0.02, 0.1),
		).toBeCloseTo(0.1);
		expect(
			effectiveRelayRate({ ...readyRate, rate: 0.12 }, 0.02, 0.1),
		).toBeNull();
	});

	it("reads semantic backend error reasons", () => {
		expect(
			relayErrorReason({ code: 500, reason: "RELAY_PROFIT_UNAVAILABLE" }),
		).toBe("RELAY_PROFIT_UNAVAILABLE");
		expect(relayErrorReason({ code: 500 })).toBeUndefined();
	});
});
