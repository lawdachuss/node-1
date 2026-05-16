/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ['./router/view/templates/**/*.html'],
  darkMode: 'class',
  theme: {
    extend: {
      fontFamily: {
        sans: ['Noto Sans TC', 'system-ui', 'sans-serif'],
      },
    },
  },
  plugins: [],
}
