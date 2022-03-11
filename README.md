# kube-proxyCommand

## usage
example in `~/.ssh/config`
```
Host develop-container
    HostName develop-container
    User root
    Port 22
    IdentityFile /path/to/private/key
    ProxyCommand kube-proxyCommand--name=podName --namespace=podNamespace --port=%p
```