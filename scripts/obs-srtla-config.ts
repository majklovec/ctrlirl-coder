export function buildSRTLATestConfig(
    source: string,
    host: string,
    port: number,
    uplinkIPs: string[],
): string {
    const cfg = Bun.TOML.parse(source);
    let srtla: object = {};
    if ("srtla" in cfg) {
        if (typeof cfg.srtla !== "object" || cfg.srtla === null || Array.isArray(cfg.srtla)) {
            throw new Error("config [srtla] must be a table");
        }
        srtla = cfg.srtla;
    }
    return Bun.TOML.stringify({
        ...cfg,
        sink: "srtla",
        srtla: {
            ...srtla,
            receiver_host: host,
            receiver_port: port,
            uplink_ips: uplinkIPs,
        },
    });
}
