window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "/docs/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: true,
    tryItOutEnabled: true,
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
  });
};
