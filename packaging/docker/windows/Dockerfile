# escape=`
# Prepared inputs only; see README.md. Windows Server 2022 builder required.
# The digest is the windows/amd64 manifest, not a floating ltsc2022 resolution.
ARG WINDOWS_BASE_IMAGE=mcr.microsoft.com/windows/servercore:ltsc2022@sha256:6b43c814ed2a800563083ce3193e5f1951d4d6a18fd2879ff45173851db82bd5
FROM ${WINDOWS_BASE_IMAGE} AS inputs
SHELL ["powershell", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]
WORKDIR C:/inputs
COPY goobers.exe goobers-operator.exe release.json SHA256SUMS mingit.zip zoneinfo.zip dependencies.json Verify-Inputs.ps1 Release-Metadata.ps1 ./
RUN .\Verify-Inputs.ps1; Expand-Archive -LiteralPath mingit.zip -DestinationPath C:\Git

FROM ${WINDOWS_BASE_IMAGE}
SHELL ["powershell", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]
COPY --from=inputs C:/Git/ C:/Git/
COPY --from=inputs ["C:/inputs/goobers.exe", "C:/inputs/goobers-operator.exe", "C:/inputs/release.json", "C:/inputs/zoneinfo.zip", "C:/inputs/dependencies.json", "C:/Goobers/"]
COPY Configure-Image.ps1 Verify-Image.ps1 Release-Metadata.ps1 C:/Goobers/
ENV PATH="C:\Goobers;C:\Git\cmd;C:\Git\usr\bin;C:\Windows\system32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0" `
    HOME="C:\Users\ContainerUser" `
    USERPROFILE="C:\Users\ContainerUser" `
    TEMP="C:\Users\ContainerUser\AppData\Local\Temp" `
    TMP="C:\Users\ContainerUser\AppData\Local\Temp" `
    ZONEINFO="C:\Goobers\zoneinfo.zip" `
    SSL_CERT_FILE="C:\Git\mingw64\etc\ssl\certs\ca-bundle.crt" `
    GIT_TERMINAL_PROMPT="0"
RUN C:\Goobers\Configure-Image.ps1
USER ContainerUser
WORKDIR C:/workspace
# Verify executable stamps, shell/git operation and writable roots as the actual user.
RUN C:\Goobers\Verify-Image.ps1
ENTRYPOINT ["goobers"]
CMD ["--version"]
