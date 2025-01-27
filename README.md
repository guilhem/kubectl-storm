# kubectl-storm

![alt text](image.png)

This `kubectl` plugin aim to detect and report a storm of changes on some resources in a Kubernetes cluster.

This happens when a resource is updated multiple times in a short period of time, which can be a sign of a misbehaving controller or a human error.

## Installation

### Krew

```bash
kubectl krew install storm
```

### Releases

Download the latest release from the [releases page](https://github.com/guilhem/kubectl-storm/releases).

## Usage

```bash
kubectl storm --generation-changes=5 --run-duration=1m
```

## Example

```text
2025/01/27 17:47:15 INFO Too many generation changes resource=pods uid=2b153a1f-994b-4018-bc5d-46303f52833b "count list"=6
2025/01/27 17:47:15 INFO Too many generation changes resource=pods uid=4f8818d4-333c-440a-b98d-74f81b09b716 "count list"=9
2025/01/27 17:47:15 INFO Too many generation changes resource=configmaps uid=588f590a-2bb2-441b-8bfc-4ee32509a41c "count list"=7
2025/01/27 17:47:15 INFO Too many generation changes resource=configmaps uid=53813605-b454-4f57-adad-b2717752c519 "count list"=7
2025/01/27 17:47:15 INFO Too many generation changes resource=configmaps uid=d34b259c-2761-46f3-b3ad-b8cec660c5e7 "count list"=7
2025/01/27 17:47:15 INFO Too many generation changes resource=jobs uid=22785e05-a2f3-450f-901c-23d8379b302f "count list"=6
```

## License

[APACHE 2.0](LICENSE)
