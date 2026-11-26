// CLI argument builder for @ctrlirl/ctrlirl-coder.
//
// Emits the ctrlirl-coder binary's long flags: --config, --pipeline,
// --scene, --dry-run, --api-addr. The legacy `-b` flag is intentionally
// never emitted (removed from the backend).
//
// Rule: if both pipelineFile and sceneFile are given, the Go binary treats
// --scene as taking precedence (scene composition overrides a flat pipeline).
// Log level is not a flag: set the CTRLIRL_LOG_LEVEL env var (debug | info |
// warning | error) via SpawnOptions.env.

export interface CtrlIRLCoderArgsOptions {
    configFile?: string; // --config
    pipelineFile?: string; // --pipeline
    sceneFile?: string; // --scene (Go-only extension)
    dryRun?: boolean; // --dry-run
    apiAddr?: string; // --api-addr (shared HTTPS API: /rpc + /metrics)
}

export function buildCtrlIRLCoderArgs(opts: CtrlIRLCoderArgsOptions): string[] {
    const args: string[] = [];
    if (opts.configFile) args.push("--config", opts.configFile);
    if (opts.pipelineFile) args.push("--pipeline", opts.pipelineFile);
    if (opts.sceneFile) args.push("--scene", opts.sceneFile); // Go extension
    if (opts.dryRun) args.push("--dry-run");
    if (opts.apiAddr) args.push("--api-addr", opts.apiAddr);
    return args;
}
