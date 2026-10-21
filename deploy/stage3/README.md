# Stage-3 controller kit

Full cluster setup and run steps, from a fresh CloudLab experiment to the
sweep with the `hc` arm: [RUNBOOK.md](RUNBOOK.md).

What the phase orchestrator (`dsb_hd_counter/.../stage3-eval/run-phased-stage3.sh`)
expects on the target node, by default in `/tmp/mitig` on node-4:

| file | from |
|---|---|
| `mitigation-controller` | built by `install-kit.sh` |
| `run-mitigation.sh` | this dir; launches the controller for one run |
| `arms/common.env`, `arms/<arm>.env` | this dir; controller settings per arm |
| `targets.yaml` | `sim/real/targets.yaml` |
| `policy-formula.yaml` | `sim/offline/policy-formula.yaml` (placeholder; formula mode bypasses CEL) |
| `kubeconfig-admin` | node-0's `~/.kube/config` |

Install or refresh it from node-0 (after a reboot, or after changing controller code):

```bash
cd /local/simple-mitigation-1 && ./deploy/stage3/install-kit.sh node-4 /tmp/mitig
```

## Arms

All arms run formula mode, disarmed until the orchestrator touches `ARM` at t0+80 s.

| arm | isolation channel (`ISO_ACTUATOR`) | scale-out (`N_MAX`) |
|---|---|---|
| `none` | none (trace only) | 0 |
| `isolate` | isolate (cpu.max budget c(t)) | 0 |
| `horizontal` | none | 3, via `search-overflow` |
| `combined` | isolate | 3 (the 2026-07-14 sweep3 setup) |
| `hc` | hc (HarvestContainers on/off on e_iso) | 0 |
| `hc-combined` | hc | 3 |

`common.env` holds the July sweep's values, read back from its controller
logs. `CAP_BASE_CORES=8` was sized for the SMT-on node; re-tune it for the
16-CPU nosmt node before comparing isolate numbers.

The 2026-07-13 "isolate" sweep (`phased-20260713`) ran with `N_MAX=3` and no
overflow deployment, so it was really isolate plus in-place scale-out. Its
numbers are not comparable to the `isolate` arm here.
