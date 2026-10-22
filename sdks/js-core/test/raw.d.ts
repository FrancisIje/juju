// Vite's ?raw suffix imports a file as a string (used for shared fixtures).
declare module "*?raw" {
  const text: string;
  export default text;
}
