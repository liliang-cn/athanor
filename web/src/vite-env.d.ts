/// <reference types="vite/client" />

// Side-effect imports of stylesheets are how Vite pulls CSS into the bundle;
// TypeScript needs to be told they are legal before it will allow one.
declare module "*.css";
