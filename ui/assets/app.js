(function () {
  "use strict";

  var bridge = window.S2PBridge;
  var form = document.getElementById("config-form");
  var message = document.getElementById("message");
  var saveButton = document.getElementById("save");
  var testButton = document.getElementById("test");
  var refreshButton = document.getElementById("refresh-status");

  var NUMBER_FIELDS = ["response_header_timeout_seconds", "idle_conn_timeout_seconds", "max_idle_conns_per_host"];
  var BOOLEAN_FIELDS = ["enable_http2", "use_account_proxy"];

  function setMessage(text, kind) {
    message.textContent = text || "";
    message.className = "message" + (kind ? " " + kind : "");
  }

  function setBusy(busy) {
    saveButton.disabled = busy;
    testButton.disabled = busy;
  }

  function fillForm(config) {
    NUMBER_FIELDS.forEach(function (name) {
      if (config[name] !== undefined) form.elements[name].value = config[name];
    });
    BOOLEAN_FIELDS.forEach(function (name) {
      if (config[name] !== undefined) form.elements[name].checked = Boolean(config[name]);
    });
  }

  function readForm() {
    var config = {};
    NUMBER_FIELDS.forEach(function (name) {
      var input = form.elements[name];
      if (!input.checkValidity()) throw new Error(input.closest("label").querySelector("span").textContent + " 取值无效");
      config[name] = Number(input.value);
    });
    BOOLEAN_FIELDS.forEach(function (name) {
      config[name] = form.elements[name].checked;
    });
    return config;
  }

  function formatDuration(seconds) {
    seconds = Number(seconds) || 0;
    var h = Math.floor(seconds / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    var s = seconds % 60;
    return (h ? h + " 小时 " : "") + (h || m ? m + " 分 " : "") + s + " 秒";
  }

  function text(id, value) {
    document.getElementById(id).textContent = value;
  }

  function renderStatus(result) {
    text("st-health", result.healthy ? "正常" : "异常" + (result.message ? "：" + result.message : ""));
    var snapshot = {};
    try { snapshot = result.status_json ? JSON.parse(result.status_json) : {}; } catch (e) { snapshot = {}; }
    text("st-version", snapshot.version || "—");
    text("st-mode", snapshot.mode === "passthrough" ? "原样透传" : (snapshot.mode || "—"));
    text("st-uptime", snapshot.uptime_seconds !== undefined ? formatDuration(snapshot.uptime_seconds) : "—");
    text("st-requests", snapshot.requests !== undefined ? snapshot.requests : "—");
    text("st-inflight", snapshot.in_flight !== undefined ? snapshot.in_flight : "—");
    text("st-failed", snapshot.failed !== undefined ? snapshot.failed : "—");
    text("st-host", snapshot.host_services === undefined ? "—" : (snapshot.host_services ? "已连接" : "未连接"));
  }

  function refreshStatus() {
    return bridge.status().then(renderStatus).catch(function (error) {
      text("st-health", "无法获取：" + error.message);
    });
  }

  function reportSize() {
    bridge.resize(document.documentElement.scrollHeight + 16);
  }

  form.addEventListener("submit", function (event) {
    event.preventDefault();
    var config;
    try {
      config = readForm();
    } catch (error) {
      setMessage(error.message, "error");
      return;
    }
    setBusy(true);
    setMessage("保存中…");
    bridge.saveConfig(config).then(function (saved) {
      fillForm(saved);
      setMessage("已保存", "success");
      return refreshStatus();
    }).catch(function (error) {
      setMessage("保存失败：" + error.message, "error");
    }).then(function () { setBusy(false); });
  });

  testButton.addEventListener("click", function () {
    setBusy(true);
    setMessage("测试中…");
    bridge.testConfig().then(function (result) {
      var latency = result.latency_ms !== undefined ? "（" + result.latency_ms + " ms）" : "";
      setMessage((result.message || "测试通过") + latency, "success");
    }).catch(function (error) {
      setMessage("测试失败：" + error.message, "error");
    }).then(function () { setBusy(false); });
  });

  refreshButton.addEventListener("click", refreshStatus);

  bridge.ready();
  setBusy(true);
  bridge.loadConfig().then(function (config) {
    fillForm(config);
    setMessage("");
  }).catch(function (error) {
    setMessage("读取配置失败：" + error.message, "error");
  }).then(function () {
    setBusy(false);
    reportSize();
  });
  refreshStatus().then(reportSize);
})();
