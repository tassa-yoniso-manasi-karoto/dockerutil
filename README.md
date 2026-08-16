This module handles Docker Compose management for my language-processing
libraries. I treat it as a private implementation library and do not recommend
depending on it unless you intend to maintain a fork.

## Process ownership

Applications that may run concurrently should create one `Runtime` per process
and put it in the context passed to service managers:

```go
runtime, err := dockerutil.NewRuntime(ctx, dockerutil.RuntimeConfig{
    Application: "my-application",
    RootDir:     runtimeStateDirectory,
})
if err != nil {
    return err
}
defer runtime.Close(context.Background())

ctx = dockerutil.WithRuntime(ctx, runtime)
```

An owned project receives a unique Compose namespace and instance scratch
directory. A shared project keeps a stable namespace and is removed only after
the final process lease is released. Docker resources are labeled and labels
are validated before automatic teardown.

The operating system releases runtime and client file locks after an abrupt
process death, including `SIGKILL`. Cleanup is therefore eventual rather than
instantaneous: the next application startup reaps stale labeled projects and
their recorded scratch directories. Persistent data and model caches must live
outside instance scratch directories.
