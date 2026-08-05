# BasePod website

The marketing/docs site for BasePod, served as a static page in `public/`.
Deployed as a BasePod app named `pod` on `base.al` (dogfooding BasePod
itself for `pod.base.al`). Build the image locally with:

```bash
podman build -t basepod-site -f site/Containerfile site
```
