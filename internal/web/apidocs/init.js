window.addEventListener('load', function () {
  window.ui = SwaggerUIBundle({
    url: '/api/openapi.yaml',
    dom_id: '#swagger-ui',
    deepLinking: true,
    persistAuthorization: false,
    tryItOutEnabled: true
  });
});
