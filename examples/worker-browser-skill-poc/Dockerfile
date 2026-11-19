# Disposable test harness only; the published Worker image stays unchanged.
FROM higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-qwenpaw-worker:v1.2.6
COPY browser-automation/ /poc/browser-automation/
COPY test_browser.py /poc/test_browser.py
COPY test_bootstrap.py /poc/test_bootstrap.py
ENTRYPOINT ["/bin/bash"]
