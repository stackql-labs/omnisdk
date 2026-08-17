
# Use cases


Broadly, the reuisite setup for excret and build here is identical to [the developer guide](/docs/developer/developer_guide.md).  We will repeat some things for easy of copy paste, but not all, refere developer guide in all cases for full clarity on build and auth.

```bash
go build -o build/omnicli ./cmd/omnicli

source cicd/vol/vendor-secrets/secrets.sh # populate according to dev guide


```
