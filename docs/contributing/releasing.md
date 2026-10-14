# Releasing

Releases are cut from annotated tags; the tag message becomes the release
notes:

```sh
git tag -a v0.1.0      # write the release notes in the editor
git push origin v0.1.0
```

Once the release is published, the workflow starts the [packages
workflow](packaging.md#package-updates), which builds the `brig` and `brig-selinux`
packages for it. Pre-releases (`v0.2.0-rc.1`) are not packaged.
