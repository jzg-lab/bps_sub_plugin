(function () {
  "use strict";

  var bridge = window.S2PBridge;
  var form = document.getElementById("config-form");
  var message = document.getElementById("message");
  var saveButton = document.getElementById("save");
  var testButton = document.getElementById("test");
  var refreshButton = document.getElementById("refresh-status");

  var NUMBER_FIELDS = ["response_header_timeout_seconds", "idle_conn_timeout_seconds", "max_idle_conns_per_host", "max_body_bytes", "tool_call_ttl_seconds", "max_image_bytes"];
  var BOOLEAN_FIELDS = ["enable_http2", "use_account_proxy", "bps_enabled", "fallback_to_codex", "tool_relay", "image_support"];
  var TEXT_FIELDS = ["model_suffix", "bps_user_agent"];

  var MODE_LABELS = { passthrough: "原样透传", basispoints_all: "basispoints（全部）", basispoints_suffix: "basispoints（按后缀）" };
  var REASON_LABELS = {
    disabled: "未启用",
    body_too_large: "请求体过大",
    bad_body: "请求体无法解析",
    model_not_allowed: "模型不在白名单",
    has_tools: "带工具",
    has_tool_history: "历史里有工具调用",
    has_attachment: "带图片或文件",
    no_authorization: "缺少令牌",
    no_account_id: "缺少账号 ID",
    model_access_changed: "模型无权限",
    blocked_html: "被 Cloudflare 拦截",
    invalid_body_422: "请求体不兼容（422）",
    connect_error: "连接失败",
    rewrite_error: "改写失败",
    error: "连接失败"
  };

  function setMessage(text, kind) {
    message.textContent = text || "";
    message.className = "message" + (kind ? " " + kind : "");
  }

  function setBusy(busy) {
    saveButton.disabled = busy;
    testButton.disabled = busy;
  }

  function fieldLabel(input) {
    var label = input.closest("label");
    var span = label && label.querySelector("span");
    return span ? span.textContent : input.name;
  }

  function fillForm(config) {
    NUMBER_FIELDS.concat(TEXT_FIELDS).forEach(function (name) {
      if (config[name] !== undefined) form.elements[name].value = config[name];
    });
    BOOLEAN_FIELDS.forEach(function (name) {
      if (config[name] !== undefined) form.elements[name].checked = Boolean(config[name]);
    });
    if (config.route_mode !== undefined) {
      Array.prototype.forEach.call(form.querySelectorAll('input[name="route_mode"]'), function (radio) {
        radio.checked = radio.value === config.route_mode;
      });
    }
    if (Array.isArray(config.models)) form.elements.models.value = config.models.join("\n");
  }

  function readForm() {
    var config = {};
    NUMBER_FIELDS.forEach(function (name) {
      var input = form.elements[name];
      if (!input.checkValidity()) throw new Error(fieldLabel(input) + " 取值无效");
      config[name] = Number(input.value);
    });
    BOOLEAN_FIELDS.forEach(function (name) {
      config[name] = form.elements[name].checked;
    });
    TEXT_FIELDS.forEach(function (name) {
      config[name] = form.elements[name].value.trim();
    });
    var mode = form.querySelector('input[name="route_mode"]:checked');
    config.route_mode = mode ? mode.value : "all";
    config.models = form.elements.models.value.split(/\r?\n/).map(function (line) { return line.trim(); }).filter(Boolean);
    if (config.bps_enabled && config.models.length === 0) throw new Error("启用 basispoints 时模型白名单不能为空");
    return config;
  }

  function formatDuration(seconds) {
    seconds = Number(seconds) || 0;
    var h = Math.floor(seconds / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    var s = seconds % 60;
    return (h ? h + " 小时 " : "") + (h || m ? m + " 分 " : "") + s + " 秒";
  }

  function formatCounts(counts) {
    var keys = Object.keys(counts || {});
    if (!keys.length) return "无";
    keys.sort(function (a, b) { return counts[b] - counts[a]; });
    return keys.map(function (key) {
      return (REASON_LABELS[key] ? REASON_LABELS[key] + "（" + key + "）" : key) + "：" + counts[key];
    }).join("；");
  }

  function text(id, value) {
    document.getElementById(id).textContent = value === undefined || value === null ? "—" : value;
  }

  function renderStatus(result) {
    text("st-health", result.healthy ? "正常" : "异常" + (result.message ? "：" + result.message : ""));
    var snapshot = {};
    try { snapshot = result.status_json ? JSON.parse(result.status_json) : {}; } catch (e) { snapshot = {}; }
    text("st-version", snapshot.version);
    text("st-mode", MODE_LABELS[snapshot.mode] || snapshot.mode);
    text("st-uptime", snapshot.uptime_seconds !== undefined ? formatDuration(snapshot.uptime_seconds) : undefined);
    text("st-requests", snapshot.requests);
    text("st-inflight", snapshot.in_flight);
    text("st-failed", snapshot.failed);
    text("st-cancelled", snapshot.cancelled);
    text("st-routed-bps", snapshot.routed_bps);
    text("st-routed-codex", snapshot.routed_codex);
    text("st-tool-relayed", snapshot.tool_relayed);
    var uploaded = snapshot.images_uploaded;
    if (uploaded !== undefined) {
      var extra = [];
      if (snapshot.images_reused) extra.push("复用 " + snapshot.images_reused);
      if (snapshot.images_omitted) extra.push("降级 " + snapshot.images_omitted);
      text("st-images-uploaded", uploaded + (extra.length ? "（" + extra.join("，") + "）" : ""));
    } else {
      text("st-images-uploaded", undefined);
    }
    text("st-host", snapshot.host_services === undefined ? undefined : (snapshot.host_services ? "已连接" : "未连接"));
    text("st-skips", formatCounts(snapshot.skip_reasons));
    text("st-fallbacks", formatCounts(snapshot.fallbacks));
    text("st-bps-status", formatCounts(snapshot.bps_status));
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
    setMessage("检查中…");
    bridge.testConfig().then(function (result) {
      var latency = result.latency_ms !== undefined ? "（" + result.latency_ms + " ms）" : "";
      setMessage((result.message || "检查通过") + latency, "success");
    }).catch(function (error) {
      setMessage("检查失败：" + error.message, "error");
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
