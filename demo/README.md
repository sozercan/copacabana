# Demo: patching images inside the cluster

`copa-incluster-demo.sh` is a [demo-magic](https://github.com/paxtonhare/demo-magic) walkthrough for people new
to Copa. Two namespaces run `nginx:1.25.3-alpine` (74 fixable OS vulnerabilities). The script then shows:

1. **What runs in the cluster:** the controller, the in-cluster registry and the Trivy server.
2. **The app:** opt-in labels, and a Trivy scan of the image.
3. **Patching:** the scan, build and verify Jobs, and the signed patch in the in-cluster registry.
4. **Rollout in `shop-dev`:** automatic approval, health checks and the soak period.
5. **The result:**
   - the Deployment still names the original image, while the pods run the patched one;
   - the swap list;
   - package versions before and after;
   - a Trivy scan showing 0 fixable vulnerabilities.
6. **`shop-prod`:** a person approves with `kubectl copa approve`.
7. **Undo:** a team annotation moves `shop-dev` back to the original image, pinned by digest.

Recording: `copa-incluster.cast` (play it with `asciinema play demo/copa-incluster.cast`). Render a GIF with `agg` (below); GIFs aren't committed.

## Run it

It needs a fresh cluster, because a second run finds the existing patch and skips the build:

```sh
CLUSTER=copa-demo hack/kind-up.sh
kubectl config use-context kind-copa   # kind create switches context; switch back if you use another cluster
KCTX=kind-copa-demo make deploy
kind get kubeconfig --name copa-demo > ~/.kube/copa-demo.kubeconfig
crane copy nginx:1.25.3-alpine localhost:5001/library/nginx:1.25.3-alpine   # the app image (skip if present)

demo/copa-incluster-demo.sh            # press ENTER to advance
demo/copa-incluster-demo.sh -n         # unattended
```

To record it:

```sh
asciinema rec --headless --window-size 190x48 -i 3 -c "env TERM=xterm-256color demo/copa-incluster-demo.sh -n" demo/copa-incluster.cast
agg --font-size 14 demo/copa-incluster.cast demo/copa-incluster.gif
```

The script also needs these on the host: `trivy`, `oras`, `yq`, `bat`, `pv`, and Docker. `KUBECONFIG_DEMO` overrides the kubeconfig path.

Clean up with `kind delete cluster --name copa-demo`.
