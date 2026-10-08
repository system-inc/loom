// Vite's ?raw import: a file's text as a string, for fixtures shared with the Go side.
declare module '*?raw' {
    const text: string;
    export default text;
}
